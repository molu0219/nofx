package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"nofx/logger"
)

// BinanceOIEndpoint is /fapi/v1/openInterest, the public Binance Futures
// endpoint that returns current open interest (in base coin units) for a
// single symbol. Weight 1 per call, no auth required.
const BinanceOIEndpoint = "https://fapi.binance.com/fapi/v1/openInterest"

// BinanceOIEnricher pulls open interest for a small candidate set in
// parallel and computes 10-min / 1h % deltas from an in-memory history ring.
//
// History is maintained per-Enricher (not per-Scanner) because the same
// enricher might enrich for multiple scanners in tests, and we want the
// history to follow the data source not the consumer. In practice there's
// one of each in production.
//
// Cost model (worst case, 100 candidates × every 10 min):
//   - 100 HTTP calls × weight 1 = 100/min, well under Binance's 1200/min.
//   - With Workers=20 and per-call timeout 2s, total enrichment takes ≤ 5s.
//   - Failures (rate limit, 5xx, timeout) are skipped silently — the entry
//     keeps its zero OI and the AIScorer just won't have OI for it.
type BinanceOIEnricher struct {
	// Workers is the parallel HTTP worker count. Default 20 — enough to
	// finish 100 calls in under 3s p99 without hammering Binance.
	Workers int
	// PerCallTimeout caps any single OI fetch. Default 2s.
	PerCallTimeout time.Duration
	// HistoryDepth is how many past OI samples to keep per symbol for delta
	// computation. Default 12 (matches scanner default → 2h lookback at 10min).
	HistoryDepth int

	// Custom HTTP endpoint (for tests). Empty → BinanceOIEndpoint.
	Endpoint string
	// HTTPClient is overridable for tests. Defaults to a 5s-timeout client.
	HTTPClient *http.Client

	mu      sync.Mutex
	history map[string][]oiSample
}

type oiSample struct {
	value float64
	at    time.Time
}

// Enrich implements Enricher. Mutates each entry's OI fields in the returned
// slice; never modifies the input slice in place.
func (b *BinanceOIEnricher) Enrich(ctx context.Context, entries []UniverseEntry) ([]UniverseEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	workers := b.Workers
	if workers <= 0 {
		workers = 20
	}
	if workers > len(entries) {
		workers = len(entries)
	}
	timeout := b.PerCallTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BinanceOIEndpoint
	}
	httpClient := b.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}

	type result struct {
		idx int
		oi  float64
		ok  bool
	}

	jobs := make(chan int, len(entries))
	results := make(chan result, len(entries))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				oi, err := fetchBinanceOI(ctx, httpClient, endpoint, entries[i].Symbol, timeout)
				if err != nil {
					results <- result{idx: i, ok: false}
					continue
				}
				results <- result{idx: i, oi: oi, ok: true}
			}
		}()
	}
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	out := make([]UniverseEntry, len(entries))
	copy(out, entries)
	now := time.Now().UTC()
	successCount := 0
	for r := range results {
		if !r.ok {
			continue
		}
		out[r.idx].OpenInterest = r.oi
		successCount++
	}

	// Update history + compute deltas. Lock once for the whole batch.
	b.mu.Lock()
	if b.history == nil {
		b.history = make(map[string][]oiSample)
	}
	depth := b.HistoryDepth
	if depth <= 0 {
		depth = 12
	}
	for i := range out {
		if out[i].OpenInterest <= 0 {
			continue
		}
		sym := out[i].Symbol
		samples := b.history[sym]

		// Compute deltas from history BEFORE appending the new sample.
		// Δ10m = vs 1 step ago, Δ1h = vs 6 steps ago.
		out[i].OIChg10m = oiPctChange(out[i].OpenInterest, samples, 1)
		out[i].OIChg1h = oiPctChange(out[i].OpenInterest, samples, 6)

		samples = append(samples, oiSample{value: out[i].OpenInterest, at: now})
		if len(samples) > depth {
			samples = samples[len(samples)-depth:]
		}
		b.history[sym] = samples
	}
	b.mu.Unlock()

	if successCount < len(entries) {
		logger.Warnf("🔭 [scanner/oi] enriched %d / %d (rest left thin)", successCount, len(entries))
	} else {
		logger.Infof("🔭 [scanner/oi] enriched %d symbols", successCount)
	}
	return out, nil
}

// oiPctChange returns ((current - samples[len-stepsBack]) / samples[len-stepsBack]) * 100,
// or 0 when there isn't enough history. samples is oldest first.
func oiPctChange(current float64, samples []oiSample, stepsBack int) float64 {
	idx := len(samples) - stepsBack
	if idx < 0 {
		return 0
	}
	prev := samples[idx].value
	if prev <= 0 {
		return 0
	}
	return ((current - prev) / prev) * 100
}

// fetchBinanceOI does one /fapi/v1/openInterest call. Returns OI in base coin
// units. Errors propagate so the caller can mark the entry as not enriched.
func fetchBinanceOI(parent context.Context, c *http.Client, endpoint, symbol string, timeout time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	url := fmt.Sprintf("%s?symbol=%s", endpoint, symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("oi status %d for %s", resp.StatusCode, symbol)
	}
	var raw struct {
		Symbol       string `json:"symbol"`
		OpenInterest string `json:"openInterest"`
		Time         int64  `json:"time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, err
	}
	oi, err := strconv.ParseFloat(raw.OpenInterest, 64)
	if err != nil {
		return 0, fmt.Errorf("oi parse %q: %w", raw.OpenInterest, err)
	}
	return oi, nil
}
