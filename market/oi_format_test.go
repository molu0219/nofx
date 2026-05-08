package market

import (
	"strings"
	"testing"
)

func TestFormatOpenInterest_NilOrZeroIsEmpty(t *testing.T) {
	if got := formatOpenInterest(nil); got != "" {
		t.Errorf("nil → expected empty, got %q", got)
	}
	if got := formatOpenInterest(&OIData{Latest: 0}); got != "" {
		t.Errorf("zero Latest → expected empty (caller should skip), got %q", got)
	}
}

func TestFormatOpenInterest_FullData(t *testing.T) {
	oi := &OIData{
		Latest:    100_000,
		Change1h:  2.5,
		Change4h:  8.1,
		Change24h: -3.2,
		History:   []float64{90_000, 91_000, 95_000, 100_000},
	}
	got := formatOpenInterest(oi)
	for _, want := range []string{
		"Open Interest:",
		"Δ1h=+2.50%",
		"Δ4h=+8.10%",
		"Δ24h=-3.20%",
		"OI series",
		"oldest→latest",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestFormatOpenInterest_NoHistorySkipsSeriesLine(t *testing.T) {
	oi := &OIData{Latest: 100_000, Change1h: 1, Change4h: 2, Change24h: 3}
	got := formatOpenInterest(oi)
	if strings.Contains(got, "OI series") {
		t.Errorf("unexpected series line when History is empty: %s", got)
	}
	if !strings.Contains(got, "Δ1h=+1.00%") {
		t.Errorf("delta should still render: %s", got)
	}
}

// pctChange-style invariants implemented inline so we don't depend on
// network in tests. Mirrors the math inside getOpenInterestData.
func TestOIPctChange_FromHistorySeries(t *testing.T) {
	hist := []float64{100, 110, 121, 132} // each step +10%
	pct := func(stepsBack int) float64 {
		idx := len(hist) - 1 - stepsBack
		if idx < 0 {
			return 0
		}
		ref := hist[idx]
		if ref <= 0 {
			return 0
		}
		return ((hist[len(hist)-1] - ref) / ref) * 100
	}
	// 1 step back: (132-121)/121 ≈ 9.09%
	if got := pct(1); got < 9 || got > 9.2 {
		t.Errorf("1-step back: got %v want ~9.09", got)
	}
	// Out of range → 0
	if got := pct(10); got != 0 {
		t.Errorf("out-of-range step should be 0, got %v", got)
	}
}
