package kernel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// binanceTickerEndpoint is the public USDT-M futures 24h ticker stats endpoint.
// Unauthenticated; rate limit is per-IP and easily within paper-trading needs.
const binanceTickerEndpoint = "https://fapi.binance.com/fapi/v1/ticker/24hr"

// binanceTopCacheTTL bounds how often we hit Binance for the universe scan.
// 5 minutes is well below the typical AI-decision cycle (3 min minimum) but
// avoids hammering the public endpoint on every tick when many traders share
// the same source.
const binanceTopCacheTTL = 5 * time.Minute

// binanceTickerHTTPTimeout is short — the API is fast and a hung request would
// stall the entire trading loop. Cycles run every few minutes; if the venue is
// slow we'd rather skip a cycle than wedge.
const binanceTickerHTTPTimeout = 8 * time.Second

// binanceTicker24hr is the subset of fields we read from the venue response.
// Binance returns a JSON array of objects keyed exactly like this.
type binanceTicker24hr struct {
	Symbol      string `json:"symbol"`
	QuoteVolume string `json:"quoteVolume"` // 24h notional volume in USDT
}

// binanceTopCache holds the last successful pull. Read-mostly so an RWMutex
// keeps the hot path (cache hit on every cycle) lock-free for readers.
type binanceTopCache struct {
	mu       sync.RWMutex
	symbols  []string  // sorted desc by quote volume, USDT-suffixed
	fetched  time.Time // zero = never fetched yet
}

var topCache = &binanceTopCache{}

// httpClient is shared so connections can be reused between cycles. Timeout
// guards a single Do call; the shared pool isn't a concurrency hazard because
// we never mutate it.
var httpClient = &http.Client{Timeout: binanceTickerHTTPTimeout}

// FetchBinanceTopByVolume returns the top `limit` USDT-M perp symbols on
// Binance Futures sorted by 24h quote volume (descending). Results are cached
// for binanceTopCacheTTL so repeated calls within the window reuse the same
// snapshot — important when several traders share the same coin source.
//
// `limit` is bounded by the caller; `0` means "use whatever's in cache" with a
// sensible upper bound of 50. We only ship the prefix the caller asked for.
func FetchBinanceTopByVolume(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50 // soft ceiling — token budget upstream caps the actual prompt size
	}

	// Cache hit fast path.
	topCache.mu.RLock()
	if !topCache.fetched.IsZero() && time.Since(topCache.fetched) < binanceTopCacheTTL && len(topCache.symbols) >= limit {
		out := append([]string(nil), topCache.symbols[:limit]...)
		topCache.mu.RUnlock()
		return out, nil
	}
	topCache.mu.RUnlock()

	// Cache miss / expired — refresh under the write lock.
	topCache.mu.Lock()
	defer topCache.mu.Unlock()

	// Re-check after acquiring write lock in case another goroutine refreshed.
	if !topCache.fetched.IsZero() && time.Since(topCache.fetched) < binanceTopCacheTTL && len(topCache.symbols) >= limit {
		return append([]string(nil), topCache.symbols[:limit]...), nil
	}

	symbols, err := fetchBinanceTickersSortedByVolume()
	if err != nil {
		// On error, return stale cache if we have one — better than failing the
		// trading cycle entirely. Only return the error when we have nothing.
		if len(topCache.symbols) > 0 {
			logger.Warnf("⚠️  Binance ticker refresh failed (%v) — reusing stale cache from %s", err, topCache.fetched.Format(time.RFC3339))
			n := limit
			if n > len(topCache.symbols) {
				n = len(topCache.symbols)
			}
			return append([]string(nil), topCache.symbols[:n]...), nil
		}
		return nil, fmt.Errorf("fetch binance top: %w", err)
	}

	topCache.symbols = symbols
	topCache.fetched = time.Now().UTC()

	n := limit
	if n > len(symbols) {
		n = len(symbols)
	}
	logger.Infof("📊 Binance top-by-volume refreshed: %d symbols cached, returning top %d (e.g. %s)",
		len(symbols), n, strings.Join(symbols[:minInt(3, n)], ", "))
	return append([]string(nil), symbols[:n]...), nil
}

// fetchBinanceTickersSortedByVolume hits the public ticker endpoint, filters
// USDT-M pairs, and returns symbols sorted by 24h quote volume (descending).
func fetchBinanceTickersSortedByVolume() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, binanceTickerEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("binance returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tickers []binanceTicker24hr
	if err := json.NewDecoder(resp.Body).Decode(&tickers); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	type entry struct {
		symbol string
		volume float64
	}
	filtered := make([]entry, 0, len(tickers))
	for _, t := range tickers {
		// Only USDT-margined pairs. Binance also lists USDC/BUSD perps; the
		// rest of NOFX assumes USDT, so filter early.
		if !strings.HasSuffix(t.Symbol, "USDT") {
			continue
		}
		// Skip leveraged tokens (UP/DOWN) and edge cases that Binance occasionally
		// lists with prefixes — top-by-volume on USDT-M won't normally surface
		// these, but the filter is cheap insurance.
		if strings.HasSuffix(t.Symbol, "UPUSDT") || strings.HasSuffix(t.Symbol, "DOWNUSDT") {
			continue
		}
		vol, err := strconv.ParseFloat(t.QuoteVolume, 64)
		if err != nil || vol <= 0 {
			continue
		}
		filtered = append(filtered, entry{symbol: t.Symbol, volume: vol})
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no valid USDT-M tickers in Binance response (received %d total)", len(tickers))
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].volume > filtered[j].volume
	})

	out := make([]string, len(filtered))
	for i, e := range filtered {
		out[i] = e.symbol
	}
	return out, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
