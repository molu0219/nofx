package scanner

import "context"

// fakeEnricher is a tiny in-memory Enricher used by scanner integration tests
// to verify the prefilter+enrich pipeline without network. Lives in its own
// _test.go file so any of the package's tests can reuse it.
type fakeEnricher struct {
	called  int
	gotSyms []string
	supply  map[string]float64 // symbol → OI
}

func (f *fakeEnricher) Enrich(_ context.Context, entries []UniverseEntry) ([]UniverseEntry, error) {
	f.called++
	out := make([]UniverseEntry, len(entries))
	copy(out, entries)
	for i, e := range out {
		f.gotSyms = append(f.gotSyms, e.Symbol)
		if oi, ok := f.supply[e.Symbol]; ok {
			out[i].OpenInterest = oi
		}
	}
	return out, nil
}
