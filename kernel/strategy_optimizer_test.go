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

func TestApplyBounded_PrimaryTimeframeAcceptsAllowedOnly(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.Klines.PrimaryTimeframe = "3m"

	bogus := "13m"
	if o.applyBounded(cfg, optimizerSuggestion{PrimaryTimeframe: &bogus}) {
		t.Fatalf("13m is not in the allowed list; should not change")
	}
	if cfg.Indicators.Klines.PrimaryTimeframe != "3m" {
		t.Fatalf("primary_timeframe mutated to invalid value: %q", cfg.Indicators.Klines.PrimaryTimeframe)
	}

	good := "15m"
	if !o.applyBounded(cfg, optimizerSuggestion{PrimaryTimeframe: &good}) {
		t.Fatalf("15m is allowed and different — expected change")
	}
	if cfg.Indicators.Klines.PrimaryTimeframe != "15m" {
		t.Fatalf("primary_timeframe got=%q want=15m", cfg.Indicators.Klines.PrimaryTimeframe)
	}
}

func TestApplyBounded_PrimaryCountClamps(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.Klines.PrimaryCount = 20
	too_high := 100
	if !o.applyBounded(cfg, optimizerSuggestion{PrimaryCount: &too_high}) {
		t.Fatalf("expected change")
	}
	if cfg.Indicators.Klines.PrimaryCount != 30 {
		t.Fatalf("primary_count clamp got=%d want=30", cfg.Indicators.Klines.PrimaryCount)
	}
}

func TestApplyBounded_SelectedTimeframesDedupAndCap(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.Klines.SelectedTimeframes = []string{"3m"}

	if !o.applyBounded(cfg, optimizerSuggestion{
		SelectedTimeframes: []string{"3m", "3m", " 15M ", "1h", "bogus", "4h", "2h"}, // 5 valid + 1 invalid
	}) {
		t.Fatalf("expected change")
	}
	got := cfg.Indicators.Klines.SelectedTimeframes
	if len(got) != 4 {
		t.Fatalf("len got=%d want=4 (cap)", len(got))
	}
	want := map[string]bool{"3m": true, "15m": true, "1h": true, "4h": true}
	for _, tf := range got {
		if !want[tf] {
			t.Fatalf("unexpected tf %q (got=%v)", tf, got)
		}
	}
	if !cfg.Indicators.Klines.EnableMultiTimeframe {
		t.Fatalf("multi-timeframe should auto-enable when len > 1")
	}
}

func TestApplyBounded_MarketDataToggles(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.EnableVolume = true
	fa := false
	if !o.applyBounded(cfg, optimizerSuggestion{EnableVolume: &fa, EnableOI: &fa, EnableFundingRate: &fa}) {
		t.Fatalf("expected change")
	}
	if cfg.Indicators.EnableVolume || cfg.Indicators.EnableOI || cfg.Indicators.EnableFundingRate {
		t.Fatalf("toggles not applied: %+v", cfg.Indicators)
	}
}

func TestApplyBounded_IndicatorPeriodsClampDedupCap(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.EMAPeriods = []int{20, 50}
	cfg.Indicators.RSIPeriods = []int{14}
	cfg.Indicators.ATRPeriods = []int{14}
	cfg.Indicators.BOLLPeriods = []int{20}

	if !o.applyBounded(cfg, optimizerSuggestion{
		EMAPeriods:  []int{500, 9, 9, 21, 50, 200}, // 500 → clamp to 200, dup 9, take first 4
		RSIPeriods:  []int{1, 7, 14, 100},          // 1 → 3, 100 → 50, max 3
		ATRPeriods:  []int{0, 7, 14, 30},           // 0 → 3, max 2
		BOLLPeriods: []int{2, 20, 50},              // 2 → 5, max 2
	}) {
		t.Fatalf("expected change")
	}

	if !sameIntSet(cfg.Indicators.EMAPeriods, []int{200, 9, 21, 50}) {
		t.Fatalf("EMA: got=%v", cfg.Indicators.EMAPeriods)
	}
	if !sameIntSet(cfg.Indicators.RSIPeriods, []int{3, 7, 14}) {
		t.Fatalf("RSI: got=%v", cfg.Indicators.RSIPeriods)
	}
	if !sameIntSet(cfg.Indicators.ATRPeriods, []int{3, 7}) {
		t.Fatalf("ATR: got=%v", cfg.Indicators.ATRPeriods)
	}
	if !sameIntSet(cfg.Indicators.BOLLPeriods, []int{5, 20}) {
		t.Fatalf("BOLL: got=%v", cfg.Indicators.BOLLPeriods)
	}
}

func TestApplyBounded_IndicatorPeriodsNoChangeOnSameSet(t *testing.T) {
	o := &StrategyOptimizer{}
	cfg := &store.StrategyConfig{}
	cfg.Indicators.EMAPeriods = []int{20, 50}

	// Same set in different order — should NOT report a change.
	if o.applyBounded(cfg, optimizerSuggestion{EMAPeriods: []int{50, 20}}) {
		t.Fatalf("same set should not report change")
	}
}

func TestNormalisePeriodList_RejectsNonsense(t *testing.T) {
	got := normalisePeriodList([]int{-5, 0, -1}, 3, 50, 4)
	// All values get clamped to 3 — only first survives dedup.
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("got %v want [3]", got)
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
