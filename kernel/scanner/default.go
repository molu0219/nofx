package scanner

import (
	"context"
	"os"
	"sync"
	"time"

	"nofx/logger"
	"nofx/mcp"
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
//
// Scorer selection (NOFX_SCANNER_SCORER):
//   - "rule"   (default) — RuleScorer; deterministic, free, fast.
//   - "ai"     — AIScorer wrapping a claudecli AIClient, RuleScorer fallback.
//                Sends a one-line-per-symbol summary of the universe and
//                asks the model to pick top-50; the larger pool gives the
//                hysteresis layer cushion when the model's picks shift.
//
// AI scoring is opt-in because the call adds 15-30s per scan and ~12k
// tokens. For tight feedback loops the RuleScorer is plenty.
func Default() *Scanner {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultMgr != nil {
		return defaultMgr
	}

	rule := NewRuleScorer(ScoringWeights{})
	scorer := Scorer(rule)
	var enricher Enricher
	if os.Getenv("NOFX_SCANNER_SCORER") == "ai" {
		client := mcp.NewAIClientByProvider("claudecli")
		if client != nil {
			scorer = &AIScorer{
				Client:   client,
				Top:      50, // larger than watchlist for hysteresis cushion
				Fallback: rule,
			}
			// Enrichment is paired with the AI scorer: cheap rule prefilter
			// narrows 561 → 100, BinanceOIEnricher fills OI/OI deltas/range_pos
			// for those 100, and the model then reads enriched rows with
			// substantially more signal per token. Rule-only path skips this
			// — RuleScorer doesn't read OI, so the API budget would be wasted.
			enricher = &BinanceOIEnricher{}
			logger.Infof("🔭 [scanner] using AI scorer (claudecli) with rule fallback + Binance OI enrichment (top 100)")
		} else {
			logger.Warnf("🔭 [scanner] NOFX_SCANNER_SCORER=ai but claudecli client unavailable — falling back to rule scorer")
		}
	}

	defaultMgr = New(Config{
		Interval:       10 * time.Minute,
		WatchlistSize:  30,
		HistoryDepth:   12,
		MissThreshold:  2,
		FetchTickers:   BinanceTickerFetcher(),
		FetchFunding:   BinanceFundingFetcher(),
		Scorer:         scorer,
		GetOpenSymbols: pendingPositionsFn,
		Enricher:       enricher,
		// PrefilterTopK defaults to 100 inside New() when Enricher is set.
	})
	go func() {
		// Background-managed lifecycle. The scanner shuts down when the
		// process exits; until then it ticks every Interval. Errors inside
		// Run are logged by the scanner itself.
		_ = defaultMgr.Run(context.Background())
	}()
	return defaultMgr
}
