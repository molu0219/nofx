package scanner

import (
	"sort"
)

// RuleScorer ranks the universe with a deterministic linear formula.
// Cheap, transparent, deterministic — the right starting point. AIScorer
// (separate file) plugs into the same interface for richer judgment.
//
// Score = sum of weighted, normalised factor magnitudes:
//   0.30 × volume share (this symbol's 24h volume / max in universe)
//   0.30 × |10-min %|
//   0.20 × |1h %|
//   0.10 × |24h %|
//   0.10 × |funding rate × 100|   (funding rate is a small number; rescale)
//
// Default weights match aggressive momentum bias: short-window deltas
// dominate, long-window only as a tie-breaker. Tunable via Config.
type RuleScorer struct {
	Weights ScoringWeights
}

// ScoringWeights controls the linear formula. Defaults applied via
// DefaultRuleWeights when zero values are supplied.
type ScoringWeights struct {
	Volume    float64
	Delta10m  float64
	Delta1h   float64
	Delta24h  float64
	Funding   float64
}

// DefaultRuleWeights is the momentum-biased starting set used when caller
// passes a zero-value ScoringWeights.
func DefaultRuleWeights() ScoringWeights {
	return ScoringWeights{
		Volume:   0.30,
		Delta10m: 0.30,
		Delta1h:  0.20,
		Delta24h: 0.10,
		Funding:  0.10,
	}
}

// NewRuleScorer applies defaults to any zero-valued weight so callers can
// pass a partial ScoringWeights and have it filled out sensibly.
func NewRuleScorer(weights ScoringWeights) *RuleScorer {
	defaults := DefaultRuleWeights()
	if weights.Volume == 0 {
		weights.Volume = defaults.Volume
	}
	if weights.Delta10m == 0 {
		weights.Delta10m = defaults.Delta10m
	}
	if weights.Delta1h == 0 {
		weights.Delta1h = defaults.Delta1h
	}
	if weights.Delta24h == 0 {
		weights.Delta24h = defaults.Delta24h
	}
	if weights.Funding == 0 {
		weights.Funding = defaults.Funding
	}
	return &RuleScorer{Weights: weights}
}

// Name implements Scorer.
func (rs *RuleScorer) Name() string { return "rule" }

// Rank implements Scorer.
func (rs *RuleScorer) Rank(entries []UniverseEntry) []string {
	if len(entries) == 0 {
		return nil
	}

	// Normalise volume to a 0-1 share of the heaviest symbol so the volume
	// weight contributes something comparable to the % deltas.
	maxVol := 0.0
	for _, e := range entries {
		if e.QuoteVolume24h > maxVol {
			maxVol = e.QuoteVolume24h
		}
	}
	if maxVol <= 0 {
		maxVol = 1
	}

	type scored struct {
		symbol string
		score  float64
	}
	out := make([]scored, 0, len(entries))
	for _, e := range entries {
		s := 0.0
		s += rs.Weights.Volume * (e.QuoteVolume24h / maxVol)
		s += rs.Weights.Delta10m * abs(e.PriceChange10m)
		s += rs.Weights.Delta1h * abs(e.PriceChange1h)
		s += rs.Weights.Delta24h * abs(e.PriceChange24h)
		s += rs.Weights.Funding * abs(e.FundingRate*100)
		out = append(out, scored{e.Symbol, s})
	}
	sort.Slice(out, func(i, j int) bool {
		// Primary: score desc. Secondary: symbol asc for stable order on ties.
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].symbol < out[j].symbol
	})

	syms := make([]string, len(out))
	for i, x := range out {
		syms[i] = x.symbol
	}
	return syms
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
