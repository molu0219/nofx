package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Kline is a single OHLCV bar. Public so the AIScorer prompt builder can
// format it; downstream indicator math (calcMACD/RSI/ATR) reads Close+High+Low.
type Kline struct {
	OpenTime  time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64 // base coin units
	CloseTime time.Time
}

// BinanceKlineEndpoint is the public futures kline endpoint. Weight 5 per
// call (limit ≤ 100), no auth required.
const BinanceKlineEndpoint = "https://fapi.binance.com/fapi/v1/klines"

// FetchBinanceKlines pulls a kline window for a single symbol. Endpoint param
// is exposed so tests can substitute a httptest server.
//
// limit must be ≤ 1500 per Binance docs; for our scanner enrichment we use
// 24 (1h × 24 = 24h) and 42 (4h × 42 ≈ 7d) — well under any cap.
func FetchBinanceKlines(parent context.Context, c *http.Client, endpoint, symbol, interval string, limit int, perCallTimeout time.Duration) ([]Kline, error) {
	ctx, cancel := context.WithTimeout(parent, perCallTimeout)
	defer cancel()
	url := fmt.Sprintf("%s?symbol=%s&interval=%s&limit=%d", endpoint, symbol, interval, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("klines status %d for %s/%s", resp.StatusCode, symbol, interval)
	}
	// Binance returns klines as an array of arrays: [openTime, open, high,
	// low, close, volume, closeTime, quoteVolume, trades, takerBuyBase,
	// takerBuyQuote, ignore]. We need indices 0,1,2,3,4,5,6.
	var raw [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]Kline, 0, len(raw))
	for _, r := range raw {
		if len(r) < 7 {
			continue
		}
		k := Kline{}
		if v, ok := r[0].(float64); ok {
			k.OpenTime = time.UnixMilli(int64(v))
		}
		k.Open = parseStringOrFloat(r[1])
		k.High = parseStringOrFloat(r[2])
		k.Low = parseStringOrFloat(r[3])
		k.Close = parseStringOrFloat(r[4])
		k.Volume = parseStringOrFloat(r[5])
		if v, ok := r[6].(float64); ok {
			k.CloseTime = time.UnixMilli(int64(v))
		}
		if k.Close <= 0 {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// parseStringOrFloat handles Binance's habit of returning numerics as strings.
func parseStringOrFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// FetchKlinesParallel runs FetchBinanceKlines for every symbol in parallel
// using a worker pool. Returns a map symbol → klines; symbols whose fetch
// failed (timeout, 5xx, parse) are simply absent from the map. Logs the
// failure count via the provided logger func.
func FetchKlinesParallel(ctx context.Context, c *http.Client, endpoint, interval string, limit, workers int, symbols []string, perCallTimeout time.Duration) map[string][]Kline {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	if endpoint == "" {
		endpoint = BinanceKlineEndpoint
	}
	if workers <= 0 {
		workers = 20
	}
	if workers > len(symbols) {
		workers = len(symbols)
	}

	type result struct {
		sym string
		kl  []Kline
		ok  bool
	}
	jobs := make(chan string, len(symbols))
	results := make(chan result, len(symbols))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sym := range jobs {
				kl, err := FetchBinanceKlines(ctx, c, endpoint, sym, interval, limit, perCallTimeout)
				if err != nil || len(kl) < 2 {
					results <- result{sym: sym, ok: false}
					continue
				}
				results <- result{sym: sym, kl: kl, ok: true}
			}
		}()
	}
	for _, s := range symbols {
		jobs <- s
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	out := make(map[string][]Kline, len(symbols))
	for r := range results {
		if r.ok {
			out[r.sym] = r.kl
		}
	}
	return out
}
