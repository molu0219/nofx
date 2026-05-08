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

// BinanceLongShortEndpoint is the top-trader position long/short ratio.
// We pick "topLongShortPositionRatio" over the global account ratio because
// position-weighted ratio reflects what the largest 20% of accounts (by
// holdings) are actually doing — much higher signal than retail account
// counts. Public, weight 1 per call.
const BinanceLongShortEndpoint = "https://fapi.binance.com/futures/data/topLongShortPositionRatio"

// LongShortRatio is one timestamped ratio sample. Ratio = longAccount/shortAccount
// (so > 1 means more long, < 1 means more short).
type LongShortRatio struct {
	Time  time.Time
	Ratio float64
}

// FetchBinanceLongShort pulls a 1-hour-period series for one symbol. The
// returned slice is oldest → latest so the AIScorer can describe trend
// (e.g. "L/S ratio rising last 4h" = whales rotating to long).
func FetchBinanceLongShort(parent context.Context, c *http.Client, endpoint, symbol string, limit int, perCallTimeout time.Duration) ([]LongShortRatio, error) {
	ctx, cancel := context.WithTimeout(parent, perCallTimeout)
	defer cancel()
	url := fmt.Sprintf("%s?symbol=%s&period=1h&limit=%d", endpoint, symbol, limit)
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
		return nil, fmt.Errorf("L/S status %d for %s", resp.StatusCode, symbol)
	}
	var raw []struct {
		Symbol         string `json:"symbol"`
		LongShortRatio string `json:"longShortRatio"`
		LongAccount    string `json:"longAccount"`
		ShortAccount   string `json:"shortAccount"`
		Timestamp      int64  `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]LongShortRatio, 0, len(raw))
	for _, r := range raw {
		ratio, err := strconv.ParseFloat(r.LongShortRatio, 64)
		if err != nil || ratio <= 0 {
			continue
		}
		out = append(out, LongShortRatio{
			Time:  time.UnixMilli(r.Timestamp),
			Ratio: ratio,
		})
	}
	return out, nil
}

// FetchLongShortParallel mirrors FetchKlinesParallel — fans out per-symbol
// fetches and returns symbol → series map. Failed fetches are silently
// dropped (caller checks for absence).
func FetchLongShortParallel(ctx context.Context, c *http.Client, endpoint string, limit, workers int, symbols []string, perCallTimeout time.Duration) map[string][]LongShortRatio {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	if endpoint == "" {
		endpoint = BinanceLongShortEndpoint
	}
	if workers <= 0 {
		workers = 20
	}
	if workers > len(symbols) {
		workers = len(symbols)
	}

	type result struct {
		sym string
		ls  []LongShortRatio
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
				ls, err := FetchBinanceLongShort(ctx, c, endpoint, sym, limit, perCallTimeout)
				if err != nil || len(ls) < 2 {
					results <- result{sym: sym, ok: false}
					continue
				}
				results <- result{sym: sym, ls: ls, ok: true}
			}
		}()
	}
	for _, s := range symbols {
		jobs <- s
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	out := make(map[string][]LongShortRatio, len(symbols))
	for r := range results {
		if r.ok {
			out[r.sym] = r.ls
		}
	}
	return out
}
