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
	if !strings.Contains(prompt, "Δ10m=") || !strings.Contains(prompt, "vol=") {
		t.Fatalf("expected delta + volume markers; got: %s", prompt[:200])
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

func TestBuildAIScorerUserPrompt_OmitsOIFieldsWhenNotEnriched(t *testing.T) {
	entries := mkUniverse("BTCUSDT", "ETHUSDT")
	prompt := buildAIScorerUserPrompt(entries, 1)
	// OI markers must NOT appear when entries weren't enriched.
	if strings.Contains(prompt, "oi=") || strings.Contains(prompt, "rangepos=") {
		t.Fatalf("unenriched prompt should omit oi/rangepos markers; got: %s", prompt)
	}
}

func TestBuildAIScorerUserPrompt_IncludesOIFieldsWhenEnriched(t *testing.T) {
	entries := mkUniverse("BTCUSDT", "ETHUSDT")
	// Enrich BTC only; ETH stays thin.
	entries[0].OpenInterest = 100_000
	entries[0].OIChg10m = 2.5
	entries[0].OIChg1h = -3.5
	entries[0].HighPrice24h = 110
	entries[0].LowPrice24h = 90
	entries[0].Price = 100
	prompt := buildAIScorerUserPrompt(entries, 1)
	if !strings.Contains(prompt, "oi=") {
		t.Fatalf("enriched prompt should include oi= marker for BTC: %s", prompt)
	}
	if !strings.Contains(prompt, "oi10m=+2.50%") {
		t.Fatalf("enriched prompt should include oi10m delta: %s", prompt)
	}
	if !strings.Contains(prompt, "rangepos=0.50") {
		t.Fatalf("range_pos = (100-90)/(110-90) = 0.50 not in prompt: %s", prompt)
	}
	// ETH had no OI → must NOT have oi= on its line. Easiest check: it should
	// only appear once (BTC's line).
	if c := strings.Count(prompt, "oi="); c != 1 {
		t.Fatalf("oi= should appear on exactly 1 line, got %d (\nprompt: %s)", c, prompt)
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
