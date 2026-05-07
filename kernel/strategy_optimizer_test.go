package kernel

import (
	"strings"
	"testing"

	"nofx/store"
)

func intPtr(v int) *int { return &v }

func TestParseOptimizerSuggestion_Plain(t *testing.T) {
	raw := `{"reasoning":"raise threshold, exclude noisy commodity perps","min_confidence":85,"excluded_coins_add":["XAGUSDT","XAUUSDT"]}`
	got, err := parseOptimizerSuggestion(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.MinConfidence == nil || *got.MinConfidence != 85 {
		t.Fatalf("min_confidence: %+v", got.MinConfidence)
	}
	if len(got.ExcludedCoinsAdd) != 2 {
		t.Fatalf("excluded_coins_add: %+v", got.ExcludedCoinsAdd)
	}
}

func TestParseOptimizerSuggestion_StripsMarkdownFence(t *testing.T) {
	raw := "```json\n{\"reasoning\":\"x\"}\n```"
	got, err := parseOptimizerSuggestion(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Reasoning != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseOptimizerSuggestion_ExtractsFromProse(t *testing.T) {
	raw := "Here are my changes: {\"reasoning\":\"keep\"} (end)"
	got, err := parseOptimizerSuggestion(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Reasoning != "keep" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseOptimizerSuggestion_RejectsEmpty(t *testing.T) {
	if _, err := parseOptimizerSuggestion(""); err == nil {
		t.Fatalf("expected error on empty body")
	}
	if _, err := parseOptimizerSuggestion("not json"); err == nil {
		t.Fatalf("expected error on non-json body")
	}
}

func TestApplyBounded_ClampsMinConfidence(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MinConfidence = 70

	// Below floor → clamps to 50
	low := 30
	if !o.applyBounded(cfg, optimizerSuggestion{MinConfidence: &low}) {
		t.Fatalf("expected change")
	}
	if cfg.RiskControl.MinConfidence != 50 {
		t.Fatalf("clamp low got=%d want=50", cfg.RiskControl.MinConfidence)
	}

	// Above ceiling → clamps to 95
	high := 250
	if !o.applyBounded(cfg, optimizerSuggestion{MinConfidence: &high}) {
		t.Fatalf("expected change")
	}
	if cfg.RiskControl.MinConfidence != 95 {
		t.Fatalf("clamp high got=%d want=95", cfg.RiskControl.MinConfidence)
	}

	// Same value → no change reported
	same := 95
	if o.applyBounded(cfg, optimizerSuggestion{MinConfidence: &same}) {
		t.Fatalf("expected no change for same value")
	}
}

func TestApplyBounded_ExcludedCoinsAddRemove(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.CoinSource.ExcludedCoins = []string{"BTCUSDT"}

	// Add SOLUSDT, remove BTCUSDT (case + whitespace tolerant)
	if !o.applyBounded(cfg, optimizerSuggestion{
		ExcludedCoinsAdd:    []string{"solusdt", " ETHUSDT "},
		ExcludedCoinsRemove: []string{"btcusdt"},
	}) {
		t.Fatalf("expected change")
	}
	got := cfg.CoinSource.ExcludedCoins
	want := map[string]bool{"SOLUSDT": true, "ETHUSDT": true}
	if len(got) != 2 {
		t.Fatalf("len got=%d, want=2 (got=%v)", len(got), got)
	}
	for _, sym := range got {
		if !want[sym] {
			t.Fatalf("unexpected symbol %q in result %v", sym, got)
		}
	}
}

func TestApplyBounded_RejectsNonUSDTSymbols(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}

	// "BTC" without USDT suffix and bare "USDT" should be rejected silently.
	o.applyBounded(cfg, optimizerSuggestion{
		ExcludedCoinsAdd: []string{"BTC", "USDT", "BTCUSDT"},
	})
	if len(cfg.CoinSource.ExcludedCoins) != 1 || cfg.CoinSource.ExcludedCoins[0] != "BTCUSDT" {
		t.Fatalf("got %v, want [BTCUSDT]", cfg.CoinSource.ExcludedCoins)
	}
}

func TestApplyBounded_AppendsCustomPromptWithStamp(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{CustomPrompt: "Original directive"}

	if !o.applyBounded(cfg, optimizerSuggestion{
		CustomPromptAppend: "Avoid commodity perps after 3 losing trades.",
	}) {
		t.Fatalf("expected change")
	}
	if !strings.HasPrefix(cfg.CustomPrompt, "Original directive") {
		t.Fatalf("original prompt lost: %q", cfg.CustomPrompt)
	}
	if !strings.Contains(cfg.CustomPrompt, "[auto-tuned ") {
		t.Fatalf("missing audit stamp: %q", cfg.CustomPrompt)
	}
	if !strings.Contains(cfg.CustomPrompt, "Avoid commodity") {
		t.Fatalf("append text missing: %q", cfg.CustomPrompt)
	}
}

func TestApplyBounded_TrimsAppendOver500Chars(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	long := strings.Repeat("a", 800)
	o.applyBounded(cfg, optimizerSuggestion{CustomPromptAppend: long})
	// The stamp + appended portion shouldn't exceed roughly 500 + boilerplate.
	if len(cfg.CustomPrompt) > 600 {
		t.Fatalf("custom prompt too long: %d chars", len(cfg.CustomPrompt))
	}
}

func TestApplyBounded_NoSuggestionMeansNoChange(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	if o.applyBounded(cfg, optimizerSuggestion{}) {
		t.Fatalf("expected no change for empty suggestion")
	}
}

func TestMaybeReview_RespectsCycleSpacing(t *testing.T) {
	// EveryNCycles==0 → disabled.
	o := &StrategyOptimizer{}
	if err := o.MaybeReview(100); err != nil {
		t.Fatalf("disabled optimizer should be no-op, got %v", err)
	}
	// EveryNCycles==5 with no AIClient → disabled (same code path).
	o = &StrategyOptimizer{EveryNCycles: 5}
	if err := o.MaybeReview(10); err != nil {
		t.Fatalf("missing AI client should no-op, got %v", err)
	}
}

func TestApplyBounded_LeverageClamps(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.BTCETHMaxLeverage = 10
	cfg.RiskControl.AltcoinMaxLeverage = 7

	high := 100
	low := 0
	if !o.applyBounded(cfg, optimizerSuggestion{
		BTCETHLeverage:  &high,
		AltcoinLeverage: &low,
	}) {
		t.Fatalf("expected change")
	}
	if cfg.RiskControl.BTCETHMaxLeverage != 25 {
		t.Fatalf("BTC/ETH leverage clamp got=%d want=25", cfg.RiskControl.BTCETHMaxLeverage)
	}
	if cfg.RiskControl.AltcoinMaxLeverage != 1 {
		t.Fatalf("altcoin leverage clamp got=%d want=1", cfg.RiskControl.AltcoinMaxLeverage)
	}
}

func TestApplyBounded_PositionRatioClamps(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	r := 99.0
	if !o.applyBounded(cfg, optimizerSuggestion{BTCETHPositionRatio: &r}) {
		t.Fatalf("expected change")
	}
	if cfg.RiskControl.BTCETHMaxPositionValueRatio != 20 {
		t.Fatalf("position ratio clamp got=%.2f want=20", cfg.RiskControl.BTCETHMaxPositionValueRatio)
	}
}

func TestApplyBounded_MaxPositionsClamps(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MaxPositions = 3
	hi := 50
	if !o.applyBounded(cfg, optimizerSuggestion{MaxPositions: &hi}) {
		t.Fatalf("expected change")
	}
	if cfg.RiskControl.MaxPositions != 10 {
		t.Fatalf("max_positions clamp got=%d want=10", cfg.RiskControl.MaxPositions)
	}
}

func TestApplyBounded_BinanceTopLimitClamps(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.CoinSource.BinanceTopLimit = 10
	hi := 999
	if !o.applyBounded(cfg, optimizerSuggestion{BinanceTopLimit: &hi}) {
		t.Fatalf("expected change")
	}
	if cfg.CoinSource.BinanceTopLimit != store.MaxCandidateCoins {
		t.Fatalf("binance_top_limit clamp got=%d want=%d", cfg.CoinSource.BinanceTopLimit, store.MaxCandidateCoins)
	}
}

func TestApplyBounded_IndicatorTogglesIndependent(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	tr, fa := true, false

	if !o.applyBounded(cfg, optimizerSuggestion{EnableEMA: &tr, EnableRSI: &tr}) {
		t.Fatalf("expected change")
	}
	if !cfg.Indicators.EnableEMA || !cfg.Indicators.EnableRSI {
		t.Fatalf("indicator flips not applied: %+v", cfg.Indicators)
	}
	// Flipping a single one back must be tracked even when others stay true.
	if !o.applyBounded(cfg, optimizerSuggestion{EnableEMA: &fa}) {
		t.Fatalf("expected change on partial flip-back")
	}
	if cfg.Indicators.EnableEMA || !cfg.Indicators.EnableRSI {
		t.Fatalf("partial flip-back failed: %+v", cfg.Indicators)
	}
}

func TestParseOptimizerSuggestion_AllNewFields(t *testing.T) {
	raw := `{
		"reasoning": "increase aggression",
		"min_confidence": 80,
		"btc_eth_max_leverage": 15,
		"altcoin_max_leverage": 12,
		"max_positions": 6,
		"btc_eth_max_position_value_ratio": 8.0,
		"altcoin_max_position_value_ratio": 4.5,
		"min_risk_reward_ratio": 2.5,
		"binance_top_limit": 20,
		"enable_ema": true,
		"enable_rsi": true
	}`
	got, err := parseOptimizerSuggestion(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BTCETHLeverage == nil || *got.BTCETHLeverage != 15 {
		t.Fatalf("BTC/ETH leverage: %+v", got.BTCETHLeverage)
	}
	if got.MaxPositions == nil || *got.MaxPositions != 6 {
		t.Fatalf("max_positions: %+v", got.MaxPositions)
	}
	if got.BinanceTopLimit == nil || *got.BinanceTopLimit != 20 {
		t.Fatalf("binance_top_limit: %+v", got.BinanceTopLimit)
	}
	if got.EnableEMA == nil || !*got.EnableEMA {
		t.Fatalf("enable_ema: %+v", got.EnableEMA)
	}
}

func TestSummariseSuggestion(t *testing.T) {
	v := 80
	out := summariseSuggestion(optimizerSuggestion{
		MinConfidence:    &v,
		ExcludedCoinsAdd: []string{"XAUUSDT"},
	})
	if !strings.Contains(out, "min_confidence=80") || !strings.Contains(out, "exclude+=XAUUSDT") {
		t.Fatalf("got %q", out)
	}
	if summariseSuggestion(optimizerSuggestion{}) != "(none)" {
		t.Fatalf("empty suggestion summary should be '(none)'")
	}
}
