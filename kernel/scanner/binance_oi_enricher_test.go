package scanner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOIServer answers /fapi/v1/openInterest like Binance does.
// supplyOI(symbol) → (oi, ok) — returning ok=false makes the server 500
// for that symbol so the enricher's failure path is testable.
func fakeOIServer(t *testing.T, supplyOI func(symbol string) (float64, bool)) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		sym := r.URL.Query().Get("symbol")
		oi, ok := supplyOI(sym)
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"symbol":%q,"openInterest":"%g","time":%d}`, sym, oi, time.Now().UnixMilli())
	}))
	return srv, &calls
}

func TestBinanceOIEnricher_FillsOIForAllSymbols(t *testing.T) {
	srv, calls := fakeOIServer(t, func(sym string) (float64, bool) {
		// Different OI per symbol so we can verify each goes to the right entry.
		switch sym {
		case "BTCUSDT":
			return 100_000, true
		case "ETHUSDT":
			return 500_000, true
		case "SOLUSDT":
			return 8_000_000, true
		}
		return 0, false
	})
	defer srv.Close()

	enricher := &BinanceOIEnricher{
		Workers:        3,
		PerCallTimeout: 1 * time.Second,
		Endpoint:       srv.URL,
	}
	entries := []UniverseEntry{
		{Symbol: "BTCUSDT", Price: 80_000},
		{Symbol: "ETHUSDT", Price: 2_300},
		{Symbol: "SOLUSDT", Price: 150},
	}

	out, err := enricher.Enrich(context.Background(), entries)
	if err != nil {
		t.Fatalf("Enrich error: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 enriched entries, got %d", len(out))
	}
	if atomic.LoadInt64(calls) != 3 {
		t.Fatalf("expected 3 HTTP calls, got %d", atomic.LoadInt64(calls))
	}
	for _, e := range out {
		if e.OpenInterest <= 0 {
			t.Errorf("%s: OI not set (%v)", e.Symbol, e.OpenInterest)
		}
	}
	if out[0].OpenInterest != 100_000 || out[1].OpenInterest != 500_000 || out[2].OpenInterest != 8_000_000 {
		t.Fatalf("OI mis-assigned: %+v", out)
	}
}

func TestBinanceOIEnricher_PartialFailureKeepsRest(t *testing.T) {
	srv, _ := fakeOIServer(t, func(sym string) (float64, bool) {
		if sym == "BROKENUSDT" {
			return 0, false
		}
		return 1234, true
	})
	defer srv.Close()

	enricher := &BinanceOIEnricher{
		Workers:        2,
		PerCallTimeout: 500 * time.Millisecond,
		Endpoint:       srv.URL,
	}
	entries := []UniverseEntry{
		{Symbol: "BTCUSDT", Price: 80_000},
		{Symbol: "BROKENUSDT", Price: 1},
		{Symbol: "ETHUSDT", Price: 2_300},
	}
	out, err := enricher.Enrich(context.Background(), entries)
	if err != nil {
		t.Fatalf("Enrich should not propagate per-symbol failures: %v", err)
	}
	// BTC + ETH succeed, BROKEN keeps zero OI.
	for _, e := range out {
		switch e.Symbol {
		case "BROKENUSDT":
			if e.OpenInterest != 0 {
				t.Errorf("BROKEN should be left thin, got OI=%v", e.OpenInterest)
			}
		default:
			if e.OpenInterest != 1234 {
				t.Errorf("%s: expected OI=1234, got %v", e.Symbol, e.OpenInterest)
			}
		}
	}
}

func TestBinanceOIEnricher_DeltasComputedFromHistory(t *testing.T) {
	// Each call returns a different OI so we can watch deltas develop.
	var oiByCall sync.Map // symbol -> next OI to return
	oiByCall.Store("BTCUSDT", []float64{100, 110, 121, 132})
	var idx int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt64(&idx, 1) - 1
		seq, _ := oiByCall.Load("BTCUSDT")
		series := seq.([]float64)
		oi := series[i%int64(len(series))]
		fmt.Fprintf(w, `{"symbol":"BTCUSDT","openInterest":"%g","time":%d}`, oi, time.Now().UnixMilli())
	}))
	defer srv.Close()

	enricher := &BinanceOIEnricher{
		Workers:        1,
		PerCallTimeout: 1 * time.Second,
		Endpoint:       srv.URL,
		HistoryDepth:   12,
	}
	in := []UniverseEntry{{Symbol: "BTCUSDT", Price: 80_000}}

	// First call: no history yet, deltas should be 0.
	out, _ := enricher.Enrich(context.Background(), in)
	if out[0].OIChg10m != 0 || out[0].OIChg1h != 0 {
		t.Fatalf("first scan should have zero deltas, got 10m=%v 1h=%v", out[0].OIChg10m, out[0].OIChg1h)
	}

	// Second call: 1 sample of history → Δ10m = (110-100)/100 = +10%.
	out, _ = enricher.Enrich(context.Background(), in)
	if out[0].OIChg10m < 9.5 || out[0].OIChg10m > 10.5 {
		t.Fatalf("second scan Δ10m: expected ~+10%%, got %v", out[0].OIChg10m)
	}

	// Third call: 2 samples → Δ10m = (121-110)/110 ≈ +10%.
	out, _ = enricher.Enrich(context.Background(), in)
	if out[0].OIChg10m < 9.5 || out[0].OIChg10m > 10.5 {
		t.Fatalf("third scan Δ10m: expected ~+10%%, got %v", out[0].OIChg10m)
	}
}

func TestBinanceOIEnricher_EmptyInputNoCalls(t *testing.T) {
	srv, calls := fakeOIServer(t, func(string) (float64, bool) { return 1, true })
	defer srv.Close()

	enricher := &BinanceOIEnricher{Endpoint: srv.URL}
	out, err := enricher.Enrich(context.Background(), nil)
	if err != nil || out != nil {
		t.Fatalf("empty input should be a no-op, got %v / %v", err, out)
	}
	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("expected 0 HTTP calls on empty input, got %d", atomic.LoadInt64(calls))
	}
}

func TestBinanceOIEnricher_ImplementsEnricher(t *testing.T) {
	var _ Enricher = (*BinanceOIEnricher)(nil)
}

func TestUniverseEntry_RangePos(t *testing.T) {
	cases := []struct {
		name string
		e    UniverseEntry
		want float64
	}{
		{"at high", UniverseEntry{Price: 100, HighPrice24h: 100, LowPrice24h: 50}, 1},
		{"at low", UniverseEntry{Price: 50, HighPrice24h: 100, LowPrice24h: 50}, 0},
		{"middle", UniverseEntry{Price: 75, HighPrice24h: 100, LowPrice24h: 50}, 0.5},
		{"degenerate band", UniverseEntry{Price: 50, HighPrice24h: 50, LowPrice24h: 50}, 0},
		{"price below low (clip)", UniverseEntry{Price: 30, HighPrice24h: 100, LowPrice24h: 50}, 0},
		{"price above high (clip)", UniverseEntry{Price: 120, HighPrice24h: 100, LowPrice24h: 50}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.e.RangePos()
			if got < c.want-0.001 || got > c.want+0.001 {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

// fakeEnricher is a tiny in-memory Enricher used by scanner integration tests
// to verify the prefilter+enrich pipeline without network.
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
