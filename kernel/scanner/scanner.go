// Package scanner runs a periodic universe-wide screen of Binance USDT-M
// perps and produces a "watchlist" — the small set of symbols traders should
// deeply analyse this period.
//
// Architecture:
//   - Universe: every USDT-M perp on Binance (~561), pulled fresh every
//     interval from the ticker REST endpoint.
//   - Features: 24h stats + 10/30/60-min deltas (computed from in-memory
//     ring buffer of past snapshots; zero extra API calls) + funding rate.
//   - Scorer: pluggable. RuleScorer uses a linear weighted formula; future
//     AIScorer can spawn Claude CLI on the universe summary and ask
//     "pick the N most interesting".
//   - Watchlist: top-N by score, with two safety rails:
//       (a) symbols of currently-open positions are always included (no
//           PnL black hole if a position drops out of the rank);
//       (b) hysteresis — a previously-watched symbol that misses the top-N
//           on a single refresh stays one more round; only after MissThreshold
//           consecutive misses does it drop.
//
// Trader engine consumes scanner.Default().Watchlist() instead of the raw
// "binance_top by 24h volume" sort when CoinSource.SourceType=="scanner".
package scanner

import (
	"context"
	"sort"
	"sync"
	"time"

	"nofx/logger"
)

// UniverseEntry is one symbol's snapshot the scanner produces. Everything
// numerical the AI / scorer might want is here so consumers don't need to
// re-fetch.
type UniverseEntry struct {
	Symbol         string    `json:"symbol"`
	Price          float64   `json:"price"`
	QuoteVolume24h float64   `json:"quote_volume_24h"`
	PriceChange24h float64   `json:"price_change_24h_pct"`
	PriceChange1h  float64   `json:"price_change_1h_pct"`
	PriceChange30m float64   `json:"price_change_30m_pct"`
	PriceChange10m float64   `json:"price_change_10m_pct"`
	HighPrice24h   float64   `json:"high_24h"`
	LowPrice24h    float64   `json:"low_24h"`
	FundingRate    float64   `json:"funding_rate"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Scorer picks the order in which symbols should populate the watchlist.
// Implementations: RuleScorer (linear formula, free), AIScorer (Claude call).
type Scorer interface {
	// Rank returns symbols sorted best-first.
	Rank(entries []UniverseEntry) []string
	// Name is used in logs.
	Name() string
}

// FetchTickersFunc returns the latest universe snapshot. Injectable so tests
// don't hit Binance and so we can swap upstream venues later.
type FetchTickersFunc func(ctx context.Context) ([]TickerSnapshot, error)

// FetchFundingFunc returns funding rate per symbol. Optional — if nil, all
// FundingRate fields stay 0.
type FetchFundingFunc func(ctx context.Context) (map[string]float64, error)

// TickerSnapshot is the rich subset of /fapi/v1/ticker/24hr we need.
type TickerSnapshot struct {
	Symbol             string
	Price              float64
	QuoteVolume24h     float64
	PriceChange24hPct  float64
	HighPrice24h       float64
	LowPrice24h        float64
}

// PositionsFunc reports symbols currently held, so the scanner can guarantee
// they stay in the watchlist regardless of score. Optional; if nil, no
// protection is applied.
type PositionsFunc func() []string

// Config controls scanner behaviour. Zero values fall back to defaults.
type Config struct {
	Interval        time.Duration // default 10 min
	WatchlistSize   int           // default 30
	HistoryDepth    int           // snapshots kept; default 12 (covers 2h at 10min)
	MissThreshold   int           // consecutive misses before drop; default 2
	FetchTickers    FetchTickersFunc
	FetchFunding    FetchFundingFunc
	GetOpenSymbols  PositionsFunc
	Scorer          Scorer
}

// Scanner is the long-running background screener.
type Scanner struct {
	cfg Config

	// History ring of price snapshots, oldest first. Keyed by symbol.
	historyMu      sync.RWMutex
	priceHistory   []map[string]float64
	historyAdded   []time.Time

	// Current watchlist + hysteresis bookkeeping.
	stateMu      sync.RWMutex
	watchlist    []string
	missStreak   map[string]int
	lastEntries  []UniverseEntry

	// Subscribers notified when the watchlist changes.
	subsMu sync.RWMutex
	subs   []chan []string
}

// New constructs a Scanner with sensible defaults applied to any unset Config
// fields. FetchTickers + Scorer are required (caller-provided).
func New(cfg Config) *Scanner {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if cfg.WatchlistSize <= 0 {
		cfg.WatchlistSize = 30
	}
	if cfg.HistoryDepth <= 0 {
		cfg.HistoryDepth = 12
	}
	if cfg.MissThreshold <= 0 {
		cfg.MissThreshold = 2
	}
	return &Scanner{
		cfg:          cfg,
		priceHistory: make([]map[string]float64, 0, cfg.HistoryDepth+1),
		historyAdded: make([]time.Time, 0, cfg.HistoryDepth+1),
		missStreak:   make(map[string]int),
	}
}

// Run blocks until ctx is cancelled. It performs an immediate refresh on
// startup so the watchlist is populated before the first trader cycle picks
// it up. Returns nil on graceful shutdown.
func (s *Scanner) Run(ctx context.Context) error {
	if s.cfg.FetchTickers == nil || s.cfg.Scorer == nil {
		return errMissingDeps
	}
	logger.Infof("🔭 [scanner] starting: interval=%s watchlist_size=%d scorer=%s",
		s.cfg.Interval, s.cfg.WatchlistSize, s.cfg.Scorer.Name())

	if err := s.Refresh(ctx); err != nil {
		logger.Warnf("🔭 [scanner] initial refresh failed: %v", err)
	}

	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.Refresh(ctx); err != nil {
				logger.Warnf("🔭 [scanner] refresh failed: %v", err)
			}
		}
	}
}

// Refresh runs a single screen. Idempotent + safe to call from outside the
// background loop (e.g. for tests, or on demand from an admin endpoint).
func (s *Scanner) Refresh(ctx context.Context) error {
	tickers, err := s.cfg.FetchTickers(ctx)
	if err != nil {
		return err
	}

	// Pull funding rates (best effort — scanner still works without them).
	var funding map[string]float64
	if s.cfg.FetchFunding != nil {
		if f, err := s.cfg.FetchFunding(ctx); err == nil {
			funding = f
		} else {
			logger.Warnf("🔭 [scanner] funding fetch failed (continuing without): %v", err)
		}
	}

	now := time.Now().UTC()
	entries := s.buildEntries(tickers, funding, now)
	s.pushHistory(now, entries)

	// Score the universe.
	ranked := s.cfg.Scorer.Rank(entries)

	// Build the new watchlist with position protection + hysteresis.
	var open []string
	if s.cfg.GetOpenSymbols != nil {
		open = s.cfg.GetOpenSymbols()
	}
	newList := s.composeWatchlist(ranked, open)

	s.stateMu.Lock()
	prev := s.watchlist
	s.watchlist = newList
	s.lastEntries = entries
	s.stateMu.Unlock()

	if !sameSymbolSet(prev, newList) {
		logger.Infof("🔭 [scanner] watchlist updated: %d symbols (e.g. %s); held protected: %d",
			len(newList), preview(newList, 5), len(open))
		s.notifySubscribers(newList)
	}
	return nil
}

// buildEntries enriches each ticker with deltas from the price history.
// Caller MUST NOT hold s.historyMu.
func (s *Scanner) buildEntries(tickers []TickerSnapshot, funding map[string]float64, now time.Time) []UniverseEntry {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()

	out := make([]UniverseEntry, 0, len(tickers))
	for _, t := range tickers {
		e := UniverseEntry{
			Symbol:         t.Symbol,
			Price:          t.Price,
			QuoteVolume24h: t.QuoteVolume24h,
			PriceChange24h: t.PriceChange24hPct,
			HighPrice24h:   t.HighPrice24h,
			LowPrice24h:    t.LowPrice24h,
			UpdatedAt:      now,
		}
		// Deltas: assumes interval = 10 min so step 1 = 10m, step 3 = 30m, step 6 = 60m.
		// If interval isn't 10m the names lie but the math still works as
		// "compared to N steps ago" — caller can rename if needed.
		e.PriceChange10m = s.deltaPctLocked(t.Symbol, t.Price, 1)
		e.PriceChange30m = s.deltaPctLocked(t.Symbol, t.Price, 3)
		e.PriceChange1h = s.deltaPctLocked(t.Symbol, t.Price, 6)
		if funding != nil {
			e.FundingRate = funding[t.Symbol]
		}
		out = append(out, e)
	}
	return out
}

// deltaPctLocked computes "current price vs price N snapshots ago" as a
// percentage. Returns 0 when there isn't enough history. Caller MUST hold
// s.historyMu (read).
func (s *Scanner) deltaPctLocked(symbol string, current float64, stepsBack int) float64 {
	idx := len(s.priceHistory) - stepsBack
	if idx < 0 || current <= 0 {
		return 0
	}
	prev, ok := s.priceHistory[idx][symbol]
	if !ok || prev <= 0 {
		return 0
	}
	return ((current - prev) / prev) * 100
}

// pushHistory appends the current snapshot to the ring buffer and trims old
// entries beyond HistoryDepth.
func (s *Scanner) pushHistory(now time.Time, entries []UniverseEntry) {
	snap := make(map[string]float64, len(entries))
	for _, e := range entries {
		snap[e.Symbol] = e.Price
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.priceHistory = append(s.priceHistory, snap)
	s.historyAdded = append(s.historyAdded, now)
	if len(s.priceHistory) > s.cfg.HistoryDepth {
		s.priceHistory = s.priceHistory[len(s.priceHistory)-s.cfg.HistoryDepth:]
		s.historyAdded = s.historyAdded[len(s.historyAdded)-s.cfg.HistoryDepth:]
	}
}

// composeWatchlist applies (a) open-position protection — held symbols are
// always present — and (b) hysteresis — a previously-watched symbol gets
// MissThreshold rounds of grace before dropping out. Caller MUST NOT hold
// s.stateMu (we lock internally).
func (s *Scanner) composeWatchlist(ranked, open []string) []string {
	target := s.cfg.WatchlistSize

	// Step 1: top-N by score.
	rankedSet := make(map[string]bool, target)
	rankedTop := make([]string, 0, target)
	for _, sym := range ranked {
		if len(rankedTop) >= target {
			break
		}
		rankedSet[sym] = true
		rankedTop = append(rankedTop, sym)
	}

	// Step 2: hysteresis — keep previously-watched symbols that just missed.
	s.stateMu.Lock()
	prev := s.watchlist
	streak := s.missStreak
	for _, sym := range prev {
		if rankedSet[sym] {
			delete(streak, sym)
			continue
		}
		streak[sym]++
		if streak[sym] < s.cfg.MissThreshold {
			rankedSet[sym] = true
			rankedTop = append(rankedTop, sym)
		} else {
			delete(streak, sym)
		}
	}
	// Drop streak entries for symbols no longer relevant at all.
	for sym := range streak {
		if !rankedSet[sym] {
			delete(streak, sym)
		}
	}
	s.stateMu.Unlock()

	// Step 3: open-position protection — always include.
	for _, sym := range open {
		if !rankedSet[sym] {
			rankedSet[sym] = true
			rankedTop = append(rankedTop, sym)
		}
	}

	return rankedTop
}

// Watchlist returns the latest top-N symbols (post-hysteresis + protection).
// Empty until the first Refresh completes.
func (s *Scanner) Watchlist() []string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	out := make([]string, len(s.watchlist))
	copy(out, s.watchlist)
	return out
}

// Snapshot returns the most recent universe scan. Useful for the AI scanner
// scorer or for diagnostics endpoints.
func (s *Scanner) Snapshot() []UniverseEntry {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	out := make([]UniverseEntry, len(s.lastEntries))
	copy(out, s.lastEntries)
	return out
}

// Subscribe returns a channel that receives the new watchlist whenever it
// changes. Buffered 4 so a slow consumer doesn't block scanner refresh.
// Cancel via the returned function.
func (s *Scanner) Subscribe() (<-chan []string, func()) {
	ch := make(chan []string, 4)
	s.subsMu.Lock()
	s.subs = append(s.subs, ch)
	s.subsMu.Unlock()
	cancel := func() {
		s.subsMu.Lock()
		for i, c := range s.subs {
			if c == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
		s.subsMu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (s *Scanner) notifySubscribers(list []string) {
	s.subsMu.RLock()
	subs := s.subs
	s.subsMu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- append([]string(nil), list...):
		default:
			// Drop if consumer is slow — they'll get the next update.
		}
	}
}

// errMissingDeps is returned from Run when caller forgot to wire required deps.
var errMissingDeps = configError("scanner: FetchTickers and Scorer are required")

type configError string

func (c configError) Error() string { return string(c) }

func sameSymbolSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

func preview(syms []string, n int) string {
	if len(syms) < n {
		n = len(syms)
	}
	out := make([]string, n)
	copy(out, syms[:n])
	return joinComma(out)
}

func joinComma(syms []string) string {
	if len(syms) == 0 {
		return ""
	}
	out := syms[0]
	for _, s := range syms[1:] {
		out += ", " + s
	}
	return out
}

// stableTopN is a helper kept for future use by AIScorer to deterministically
// fall back when the AI returns fewer than TopN symbols. Currently unused.
var _ = sort.Sort
