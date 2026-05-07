package scanner

import "context"

// Enricher fills heavier per-symbol fields (open interest, OI deltas, and
// later 1m-kline-derived indicators) for a small set of pre-selected
// candidates. Decoupled from the universe fetcher because it has a much
// higher per-symbol cost and is only worth running on the prefilter winners.
//
// Contract:
//   - Input: typically 100 entries — the top K by RuleScorer.
//   - Output: same symbols, with OpenInterest + OIChg* (and any future
//     indicator fields) populated. Symbols that fail enrichment may be
//     dropped or returned with zero values; callers must tolerate either.
//   - Best-effort: implementations should not block the scanner if a few
//     individual symbol fetches fail. Return what you got + log the rest.
//   - Idempotent: calling Enrich twice with the same input must be safe;
//     stateful implementations (e.g. an OI history ring) update internal
//     state but return consistent output.
type Enricher interface {
	Enrich(ctx context.Context, entries []UniverseEntry) ([]UniverseEntry, error)
}
