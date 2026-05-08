package scanner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/mcp"
)

// fakeAIClient is a minimal mcp.AIClient implementation for testing the
// scorer in isolation. We only ever call CallWithMessages from AIScorer, so
// the other interface methods are satisfied with stubs.
type fakeAIClient struct {
	response string
	err      error
	calls    int
}

func (f *fakeAIClient) SetAPIKey(_, _, _ string)    {}
func (f *fakeAIClient) SetTimeout(_ time.Duration)  {}
func (f *fakeAIClient) CallWithMessages(_, _ string) (string, error) {
	f.calls++
	return f.response, f.err
}
func (f *fakeAIClient) CallWithRequest(_ *mcp.Request) (string, error) {
	return f.response, f.err
}
func (f *fakeAIClient) CallWithRequestStream(_ *mcp.Request, _ func(string)) (string, error) {
	return f.response, f.err
}
func (f *fakeAIClient) CallWithRequestFull(_ *mcp.Request) (*mcp.LLMResponse, error) {
	return &mcp.LLMResponse{Content: f.response}, f.err
}

func mkUniverse(symbols ...string) []UniverseEntry {
	out := make([]UniverseEntry, 0, len(symbols))
	for i, s := range symbols {
		out = append(out, UniverseEntry{
			Symbol:         s,
			Price:          float64(i+1) * 100,
			QuoteVolume24h: float64(len(symbols)-i) * 1_000_000,
		})
	}
	return out
}

func TestAIScorer_RanksFromCleanResponse(t *testing.T) {
	universe := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT", "DOGEUSDT", "XRPUSDT")
	a := &AIScorer{
		Client:   &fakeAIClient{response: "DOGEUSDT, SOLUSDT, BTCUSDT"},
		Top:      3,
		Fallback: NewRuleScorer(ScoringWeights{}),
	}
	ranked := a.Rank(universe)
	// AI picks should come first in order
	if ranked[0] != "DOGEUSDT" || ranked[1] != "SOLUSDT" || ranked[2] != "BTCUSDT" {
		t.Fatalf("AI order lost: %v", ranked)
	}
	// Tail comes from rule scorer for symbols AI didn't pick
	tail := ranked[3:]
	tailSet := map[string]bool{}
	for _, s := range tail {
		tailSet[s] = true
	}
	if !tailSet["ETHUSDT"] || !tailSet["XRPUSDT"] {
		t.Fatalf("tail missing rule-scored unselected symbols: %v", tail)
	}
}

func TestAIScorer_FallbackOnAIError(t *testing.T) {
	universe := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT")
	a := &AIScorer{
		Client:   &fakeAIClient{err: errors.New("network down")},
		Top:      3,
		Fallback: NewRuleScorer(ScoringWeights{}),
	}
	ranked := a.Rank(universe)
	if len(ranked) != 3 {
		t.Fatalf("fallback should rank entire universe, got %d", len(ranked))
	}
	// Rule scorer ranks by volume desc; mkUniverse made BTCUSDT highest vol
	if ranked[0] != "BTCUSDT" {
		t.Fatalf("fallback expected BTC first, got %v", ranked)
	}
}

func TestAIScorer_FallbackOnTooFewSymbols(t *testing.T) {
	// AI returns only 1 symbol but we asked for 4 → < half → fallback.
	universe := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT", "DOGEUSDT", "XRPUSDT")
	a := &AIScorer{
		Client:   &fakeAIClient{response: "BTCUSDT"},
		Top:      4,
		Fallback: NewRuleScorer(ScoringWeights{}),
	}
	ranked := a.Rank(universe)
	if len(ranked) != 5 {
		t.Fatalf("fallback should expose full universe, got %d", len(ranked))
	}
	// Rule output (volume desc) — BTC first
	if ranked[0] != "BTCUSDT" {
		t.Fatalf("fallback head: %v", ranked)
	}
}

func TestAIScorer_DropsHallucinatedSymbols(t *testing.T) {
	universe := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT", "DOGEUSDT", "XRPUSDT")
	a := &AIScorer{
		Client:   &fakeAIClient{response: "FAKEUSDT, BTCUSDT, NOPEUSDT, ETHUSDT, MOREJUNK, SOLUSDT"},
		Top:      3,
		Fallback: NewRuleScorer(ScoringWeights{}),
	}
	ranked := a.Rank(universe)
	// Only the 3 real ones survive at the head
	if ranked[0] != "BTCUSDT" || ranked[1] != "ETHUSDT" || ranked[2] != "SOLUSDT" {
		t.Fatalf("hallucinations not stripped: %v", ranked)
	}
}

func TestAIScorer_CachesWithinTTL(t *testing.T) {
	client := &fakeAIClient{response: "BTCUSDT, ETHUSDT, SOLUSDT"}
	a := &AIScorer{
		Client:   client,
		Top:      3,
		Fallback: NewRuleScorer(ScoringWeights{}),
		CacheTTL: time.Hour,
	}
	universe := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT", "DOGEUSDT", "XRPUSDT")
	a.Rank(universe)
	a.Rank(universe)
	a.Rank(universe)
	if client.calls != 1 {
		t.Fatalf("expected 1 AI call (cache hit on subsequent), got %d", client.calls)
	}
}

func TestParseAISymbolResponse_HandlesNoise(t *testing.T) {
	universe := map[string]bool{"BTCUSDT": true, "ETHUSDT": true, "SOLUSDT": true}

	cases := map[string]string{
		"plain commas":         "BTCUSDT, ETHUSDT, SOLUSDT",
		"newlines":             "BTCUSDT\nETHUSDT\nSOLUSDT",
		"mixed":                " BTCUSDT;ETHUSDT,\nSOLUSDT",
		"with prose":           "Here are picks: BTCUSDT, ETHUSDT, SOLUSDT.",
		"with backticks":       "`BTCUSDT`, `ETHUSDT`, `SOLUSDT`",
		"with hyphens (drop)":  "BTC-USDT, ETH-USDT, SOL-USDT", // note: parser strips hyphens → BTCUSDT etc
	}
	for name, raw := range cases {
		got := parseAISymbolResponse(raw, universe)
		if len(got) != 3 {
			t.Errorf("%s: got %d symbols, want 3 (input %q parsed to %v)", name, len(got), raw, got)
		}
	}
}

func TestParseAISymbolResponse_DedupesAndUppercases(t *testing.T) {
	universe := map[string]bool{"BTCUSDT": true, "ETHUSDT": true}
	got := parseAISymbolResponse("btcusdt, BTCUSDT, ETHUSDT, ethusdt", universe)
	if len(got) != 2 {
		t.Fatalf("dedupe failed: %v", got)
	}
}

func TestBuildAIScorerUserPrompt_Compact(t *testing.T) {
	entries := mkUniverse("BTCUSDT", "ETHUSDT", "SOLUSDT")
	for i := range entries {
		entries[i].PriceChange10m = float64(i) * 0.5
		entries[i].QuoteVolume24h = float64(1_000_000 + i*1000)
	}
	prompt := buildAIScorerUserPrompt(entries, 2)

	if !strings.Contains(prompt, "BTCUSDT") {
		t.Fatalf("BTC missing from prompt")
	}
	if !strings.Contains(prompt, "Δ10m=") || !strings.Contains(prompt, "Vol24h=") {
		t.Fatalf("expected delta + Vol24h markers; got: %s", prompt[:200])
	}
	if !strings.Contains(prompt, "Pick the top 2") || !strings.Contains(prompt, "Output 2") {
		t.Fatalf("expected top/output markers; got: %s", prompt[:300])
	}
}

func TestFormatVolume_Brackets(t *testing.T) {
	cases := map[float64]string{
		2_500_000_000: "2.5B",
		850_000_000:   "850M",
		42_000_000:    "42M",
		5_000:         "5K",
		200:           "200",
	}
	for v, want := range cases {
		if got := formatVolume(v); got != want {
			t.Errorf("formatVolume(%g): got %s want %s", v, got, want)
		}
	}
}

func TestBuildAIScorerUserPrompt_OmitsHeavyFieldsWhenNotEnriched(t *testing.T) {
	entries := mkUniverse("BTCUSDT", "ETHUSDT")
	prompt := buildAIScorerUserPrompt(entries, 1)
	// Heavy markers must NOT appear when entries weren't enriched: no OI
	// section, no 1h/4h indicator lines, no L/S line.
	for _, marker := range []string{"OI=", "1h: MACD", "4h: MACD", "L/S(top)"} {
		if strings.Contains(prompt, marker) {
			t.Errorf("unenriched prompt should omit %q; got:\n%s", marker, prompt)
		}
	}
}

func TestBuildAIScorerUserPrompt_IncludesRichFieldsWhenEnriched(t *testing.T) {
	entries := mkUniverse("BTCUSDT", "ETHUSDT")
	// Enrich BTC fully; ETH stays thin.
	entries[0].Price = 100
	entries[0].HighPrice24h = 110
	entries[0].LowPrice24h = 90
	entries[0].OpenInterest = 100_000
	entries[0].OIHistory = []float64{95_000, 96_000, 99_000, 100_000}
	entries[0].OIChg1h = 1
	entries[0].OIChg4h = 5.3
	entries[0].OIChg24h = 5.3
	// Need ≥ 26 candles for indicator block to render.
	klines := make([]Kline, 30)
	for i := range klines {
		klines[i] = Kline{Open: 100, High: 102, Low: 98, Close: 101 + float64(i)*0.1}
	}
	entries[0].Klines1h = klines
	entries[0].MACD1h = 0.0125
	entries[0].RSI1h = 62.5
	entries[0].ATR1h = 1.42
	entries[0].LongShortHistory = []LongShortRatio{{Ratio: 1.2}, {Ratio: 1.4}}
	entries[0].LongShortLatest = 1.4

	prompt := buildAIScorerUserPrompt(entries, 1)

	for _, want := range []string{
		"=== BTCUSDT ===",
		"OI=",
		"Δ1h=+1.00%",
		"1h: MACD",
		"RSI14=62.5",
		"L/S(top)=1.400",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected %q in prompt; got:\n%s", want, prompt)
		}
	}

	// ETH had no OI / no klines → its block should NOT carry the heavy
	// markers (only the header). Count "1h: MACD" should be exactly 1.
	if c := strings.Count(prompt, "1h: MACD"); c != 1 {
		t.Errorf("1h: MACD should appear exactly once (only enriched BTC), got %d", c)
	}
}

func TestFormatEntryBlock_BypassTagged(t *testing.T) {
	e := mkUniverse("PUMPUSDT")[0]
	e.ByPass = true
	got := formatEntryBlock(e)
	if !strings.Contains(got, "=== PUMPUSDT (bypass) ===") {
		t.Fatalf("bypass tag missing: %s", got)
	}
}

func TestCompressFloats(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		n      int
		want   string
	}{
		{"empty", nil, 6, ""},
		{"shorter than n", []float64{1, 2, 3}, 6, "1.00,2.00,3.00"},
		{"evenly spaced", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, 4, "1.00,4.00,8.00,12.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := compressFloats(c.values, c.n)
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// Sanity: scorer interface is satisfied.
func TestAIScorer_ImplementsScorer(t *testing.T) {
	var _ Scorer = (*AIScorer)(nil)
	a := &AIScorer{Client: &fakeAIClient{response: ""}, Fallback: NewRuleScorer(ScoringWeights{})}
	if a.Name() != "ai" {
		t.Fatalf("name: %q", a.Name())
	}
}

// Quick sanity on the unused fmt import path (catches IDE auto-removals).
var _ = fmt.Sprintf
