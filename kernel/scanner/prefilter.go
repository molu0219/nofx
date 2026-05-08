package scanner

import "sort"

// VolumeBypassPrefilter implements the Scanner v2 candidate selection rule:
//
//   1. Sort full universe by 24h quote volume desc, take the top VolumeTopK.
//   2. Add any symbol with |Δ1h| ≥ BypassThreshold (e.g. 10%) that wasn't
//      already in the top-volume set — these are momentum bypass candidates.
//   3. Cap the union at MaxCandidates so deep enrichment cost stays bounded.
//
// Implements the Scorer interface so it can drop into Scanner.cfg.Prefilter
// without changing the Refresh flow. Symbols flagged by the bypass branch
// are returned with ByPass=true (the Scanner copies that flag through to
// the entry it stores).
type VolumeBypassPrefilter struct {
	VolumeTopK       int     // default 100
	BypassThreshold  float64 // |Δ1h| in pct, default 10
	MaxCandidates    int     // default 120 (= VolumeTopK + small bypass cushion)
}

// Name implements Scorer.
func (p *VolumeBypassPrefilter) Name() string { return "volume+bypass" }

// Rank returns symbols in this order: top-volume first (volume desc), then
// bypass picks (|Δ1h| desc), trimmed to MaxCandidates.
func (p *VolumeBypassPrefilter) Rank(entries []UniverseEntry) []string {
	topK := p.VolumeTopK
	if topK <= 0 {
		topK = 100
	}
	bypass := p.BypassThreshold
	if bypass <= 0 {
		bypass = 10
	}
	maxC := p.MaxCandidates
	if maxC <= 0 {
		maxC = topK + 20
	}

	// Step 1: sort by volume desc, take top K.
	byVol := append([]UniverseEntry(nil), entries...)
	sort.Slice(byVol, func(i, j int) bool {
		return byVol[i].QuoteVolume24h > byVol[j].QuoteVolume24h
	})
	if len(byVol) > topK {
		byVol = byVol[:topK]
	}
	picked := make(map[string]bool, topK+20)
	out := make([]string, 0, maxC)
	for _, e := range byVol {
		if !picked[e.Symbol] {
			picked[e.Symbol] = true
			out = append(out, e.Symbol)
		}
	}

	// Step 2: |Δ1h| ≥ threshold bypass — sorted by absolute move so the
	// strongest movers come first within the bypass tail.
	bypassPool := make([]UniverseEntry, 0, len(entries))
	for _, e := range entries {
		if picked[e.Symbol] {
			continue
		}
		if absFloat(e.PriceChange1h) >= bypass {
			bypassPool = append(bypassPool, e)
		}
	}
	sort.Slice(bypassPool, func(i, j int) bool {
		return absFloat(bypassPool[i].PriceChange1h) > absFloat(bypassPool[j].PriceChange1h)
	})
	for _, e := range bypassPool {
		if len(out) >= maxC {
			break
		}
		out = append(out, e.Symbol)
	}
	return out
}

// BypassSymbols reports which symbols would be selected via the |Δ1h| ≥ N%
// bypass branch (i.e. NOT in the top-volume slice). Exposed so the Scanner
// can mark them on the corresponding UniverseEntry for downstream UX.
func (p *VolumeBypassPrefilter) BypassSymbols(entries []UniverseEntry) []string {
	topK := p.VolumeTopK
	if topK <= 0 {
		topK = 100
	}
	bypass := p.BypassThreshold
	if bypass <= 0 {
		bypass = 10
	}

	byVol := append([]UniverseEntry(nil), entries...)
	sort.Slice(byVol, func(i, j int) bool {
		return byVol[i].QuoteVolume24h > byVol[j].QuoteVolume24h
	})
	if len(byVol) > topK {
		byVol = byVol[:topK]
	}
	inTop := make(map[string]bool, topK)
	for _, e := range byVol {
		inTop[e.Symbol] = true
	}

	out := []string{}
	for _, e := range entries {
		if inTop[e.Symbol] {
			continue
		}
		if absFloat(e.PriceChange1h) >= bypass {
			out = append(out, e.Symbol)
		}
	}
	return out
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
