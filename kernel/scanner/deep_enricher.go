package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"nofx/logger"
)

// jsonDecode is a thin wrapper over json.NewDecoder so the call site stays
// readable. Returns wrapped error to make root-cause diagnosis easier when
// Binance changes its response shape.
func jsonDecode(r io.Reader, v interface{}) error {
	if err := json.NewDecoder(r).Decode(v); err != nil {
		return fmt.Errorf("json decode: %w", err)
	}
	return nil
}

var errEmptyOI = errors.New("oi history: empty response")

// errOIHTTP wraps a non-200 OI response so log scanning can pick it up.
type errOIHTTP struct {
	code int
	sym  string
}

func (e errOIHTTP) Error() string {
	return fmt.Sprintf("oi http %d for %s", e.code, e.sym)
}

// DeepEnricher runs the four parallel fetches the AI scorer wants per
// candidate symbol — OI history, 1h klines, 4h klines, top-trader L/S —
// and computes derived indicators (MACD, RSI, ATR) inline. Replaces the
// older OI-only enricher.
//
// All fetches are best-effort: a missing 4h kline shouldn't kill the entry,
// it just means the AI's prompt for that coin will lack 4h indicators.
// The entry is still returned so OI / 1h indicators can carry the weight.
//
// Cost (per scan, 100 candidates):
//   - 100 × OI hist call (weight 1)  = 100 weight
//   - 100 × 1h kline call (weight 5) = 500 weight
//   - 100 × 4h kline call (weight 5) = 500 weight
//   - 100 × L/S call (weight 1)      = 100 weight
//   = 1,200 weight total — at the Binance /fapi cap if all in one minute,
//     but spread across our 3 endpoint families (fapi, futures/data) the
//     real ceiling is much higher. With Workers=20 the whole batch
//     finishes in ~10-15 s wall time, well inside any minute window.
type DeepEnricher struct {
	// Workers caps parallelism per endpoint family. Default 20.
	Workers int
	// PerCallTimeout per individual HTTP call. Default 4 seconds — long
	// enough to absorb a brief network hiccup, short enough not to stall
	// the whole scan.
	PerCallTimeout time.Duration
	// HTTPClient shared across all four fetcher families. Defaults to a
	// 10s-timeout client with the Go default transport (connection reuse).
	HTTPClient *http.Client

	// Endpoints (overridable for tests).
	OIEndpoint        string // /futures/data/openInterestHist
	KlineEndpoint     string // /fapi/v1/klines
	LongShortEndpoint string // /futures/data/topLongShortPositionRatio

	// History depth for kline + L/S series. Defaults: 1h × 24, 4h × 42, L/S × 24.
	Klines1hLimit  int
	Klines4hLimit  int
	LongShortLimit int

	// OI history is maintained internally (the OI call returns its own
	// history series; we cache the latest enriched output for diagnostics).
	mu       sync.Mutex
	lastSeen map[string]time.Time
}

// Enrich implements Enricher. Returns input entries with OI / kline /
// indicator / L/S fields populated where fetches succeeded; failed fetches
// leave the corresponding field at its zero value.
func (d *DeepEnricher) Enrich(ctx context.Context, entries []UniverseEntry) ([]UniverseEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	workers := d.Workers
	if workers <= 0 {
		workers = 20
	}
	timeout := d.PerCallTimeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	httpClient := d.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	limit1h := d.Klines1hLimit
	if limit1h <= 0 {
		limit1h = 24
	}
	limit4h := d.Klines4hLimit
	if limit4h <= 0 {
		limit4h = 42
	}
	limitLS := d.LongShortLimit
	if limitLS <= 0 {
		limitLS = 24
	}

	symbols := make([]string, len(entries))
	for i, e := range entries {
		symbols[i] = e.Symbol
	}

	// Fan out the four fetch families in parallel — they share the same
	// http.Client (connection pool) but otherwise don't block each other.
	var (
		oiResult map[string]oiSnapshot
		k1hMap   map[string][]Kline
		k4hMap   map[string][]Kline
		lsMap    map[string][]LongShortRatio
		fwg      sync.WaitGroup
	)
	fwg.Add(4)
	go func() {
		defer fwg.Done()
		oiResult = fetchOIHistoryParallel(ctx, httpClient, d.OIEndpoint, workers, symbols, timeout)
	}()
	go func() {
		defer fwg.Done()
		k1hMap = FetchKlinesParallel(ctx, httpClient, d.KlineEndpoint, "1h", limit1h, workers, symbols, timeout)
	}()
	go func() {
		defer fwg.Done()
		k4hMap = FetchKlinesParallel(ctx, httpClient, d.KlineEndpoint, "4h", limit4h, workers, symbols, timeout)
	}()
	go func() {
		defer fwg.Done()
		lsMap = FetchLongShortParallel(ctx, httpClient, d.LongShortEndpoint, limitLS, workers, symbols, timeout)
	}()
	fwg.Wait()

	out := make([]UniverseEntry, len(entries))
	copy(out, entries)
	now := time.Now().UTC()

	oiOK, k1hOK, k4hOK, lsOK := 0, 0, 0, 0
	for i := range out {
		sym := out[i].Symbol
		if snap, ok := oiResult[sym]; ok && snap.latest > 0 {
			out[i].OpenInterest = snap.latest
			out[i].OIHistory = snap.history
			out[i].OIChg1h = snap.chg1h
			out[i].OIChg4h = snap.chg4h
			out[i].OIChg24h = snap.chg24h
			oiOK++
		}
		if k := k1hMap[sym]; len(k) >= 26 {
			out[i].Klines1h = k
			out[i].MACD1h = calcMACDLine(k)
			out[i].RSI1h = calcRSI(k, 14)
			out[i].ATR1h = calcATR(k, 14)
			k1hOK++
		}
		if k := k4hMap[sym]; len(k) >= 26 {
			out[i].Klines4h = k
			out[i].MACD4h = calcMACDLine(k)
			out[i].RSI4h = calcRSI(k, 14)
			out[i].ATR4h = calcATR(k, 14)
			k4hOK++
		}
		if ls := lsMap[sym]; len(ls) > 0 {
			out[i].LongShortHistory = ls
			out[i].LongShortLatest = ls[len(ls)-1].Ratio
			lsOK++
		}
	}

	d.mu.Lock()
	if d.lastSeen == nil {
		d.lastSeen = make(map[string]time.Time)
	}
	for _, e := range out {
		d.lastSeen[e.Symbol] = now
	}
	d.mu.Unlock()

	logger.Infof("🔭 [scanner/deep] enriched %d candidates: OI=%d/100 k1h=%d k4h=%d L/S=%d",
		len(entries), oiOK, k1hOK, k4hOK, lsOK)
	return out, nil
}

// oiSnapshot bundles what fetchOIHistoryParallel returns per symbol.
type oiSnapshot struct {
	latest  float64
	history []float64
	chg1h   float64
	chg4h   float64
	chg24h  float64
}

// fetchOIHistoryParallel calls /futures/data/openInterestHist for every
// symbol in parallel and computes 1h / 4h / 24h deltas from the response.
// Different from BinanceOIEnricher: that one returned just the snapshot and
// computed deltas from its own internal history ring; this one trusts
// Binance's hourly history series (24 points = 24h lookback). Simpler.
func fetchOIHistoryParallel(parent context.Context, c *http.Client, endpoint string, workers int, symbols []string, perCallTimeout time.Duration) map[string]oiSnapshot {
	if endpoint == "" {
		endpoint = "https://fapi.binance.com/futures/data/openInterestHist"
	}
	if workers <= 0 {
		workers = 20
	}
	if workers > len(symbols) {
		workers = len(symbols)
	}

	type result struct {
		sym  string
		snap oiSnapshot
		ok   bool
	}
	jobs := make(chan string, len(symbols))
	results := make(chan result, len(symbols))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sym := range jobs {
				snap, err := fetchOIHistorySingle(parent, c, endpoint, sym, perCallTimeout)
				if err != nil || snap.latest <= 0 {
					results <- result{sym: sym, ok: false}
					continue
				}
				results <- result{sym: sym, snap: snap, ok: true}
			}
		}()
	}
	for _, s := range symbols {
		jobs <- s
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	out := make(map[string]oiSnapshot, len(symbols))
	for r := range results {
		if r.ok {
			out[r.sym] = r.snap
		}
	}
	return out
}

// fetchOIHistorySingle pulls 24 hourly OI samples and turns them into a
// snapshot bundle (latest + history slice + 1h/4h/24h % deltas).
func fetchOIHistorySingle(parent context.Context, c *http.Client, endpoint, symbol string, perCallTimeout time.Duration) (oiSnapshot, error) {
	ctx, cancel := context.WithTimeout(parent, perCallTimeout)
	defer cancel()
	url := endpoint + "?symbol=" + symbol + "&period=1h&limit=24"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return oiSnapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return oiSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oiSnapshot{}, errOIHTTP{code: resp.StatusCode, sym: symbol}
	}
	var raw []struct {
		SumOpenInterest string `json:"sumOpenInterest"`
		Timestamp       int64  `json:"timestamp"`
	}
	if err := jsonDecode(resp.Body, &raw); err != nil {
		return oiSnapshot{}, err
	}
	hist := make([]float64, 0, len(raw))
	for _, r := range raw {
		v := parseStringOrFloat(r.SumOpenInterest)
		if v <= 0 {
			continue
		}
		hist = append(hist, v)
	}
	if len(hist) == 0 {
		return oiSnapshot{}, errEmptyOI
	}
	latest := hist[len(hist)-1]
	pct := func(stepsBack int) float64 {
		idx := len(hist) - 1 - stepsBack
		if idx < 0 {
			return 0
		}
		ref := hist[idx]
		if ref <= 0 {
			return 0
		}
		return ((latest - ref) / ref) * 100
	}
	return oiSnapshot{
		latest:  latest,
		history: hist,
		chg1h:   pct(1),
		chg4h:   pct(4),
		chg24h:  pct(23),
	}, nil
}
