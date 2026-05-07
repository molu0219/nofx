package scanner

import (
	"context"
	"sync"
	"time"
)

// defaultMgr is the process-wide scanner singleton consumed by trader engines
// when a strategy uses CoinSource.SourceType == "scanner". Lazily started on
// first access so tests + standalone tools that don't need the scanner pay
// nothing for it.
//
// Configuration is intentionally hard-coded for V1: 10-min interval, 30
// watchlist, 12 history depth (2h lookback), MissThreshold=2 (1 grace round).
// These can become tunable via env vars or a kernel.NewWithConfig later if
// users need per-deployment overrides; right now everyone wants the same
// thing.
var (
	defaultMu  sync.Mutex
	defaultMgr *Scanner
)

// SetOpenPositionsResolver wires a callback that returns "symbols currently
// held across all running traders" — used to guarantee those symbols stay in
// the watchlist regardless of score. Call once during process startup before
// the first Default() lookup; later calls overwrite the resolver.
//
// nil clears the protection (acceptable for tests; real deployments should
// always set this).
func SetOpenPositionsResolver(fn PositionsFunc) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	pendingPositionsFn = fn
	if defaultMgr != nil {
		defaultMgr.cfg.GetOpenSymbols = fn
	}
}

var pendingPositionsFn PositionsFunc

// Default returns the singleton scanner, starting its background goroutine
// on first access. Subsequent calls return the same instance.
func Default() *Scanner {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultMgr != nil {
		return defaultMgr
	}
	defaultMgr = New(Config{
		Interval:       10 * time.Minute,
		WatchlistSize:  30,
		HistoryDepth:   12,
		MissThreshold:  2,
		FetchTickers:   BinanceTickerFetcher(),
		FetchFunding:   BinanceFundingFetcher(),
		Scorer:         NewRuleScorer(ScoringWeights{}),
		GetOpenSymbols: pendingPositionsFn,
	})
	go func() {
		// Background-managed lifecycle. The scanner shuts down when the
		// process exits; until then it ticks every Interval. Errors inside
		// Run are logged by the scanner itself.
		_ = defaultMgr.Run(context.Background())
	}()
	return defaultMgr
}
