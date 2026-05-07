package paper

import (
	"context"
	"time"

	"nofx/logger"
	"nofx/market/stream"
)

// streamSource is the optional dependency that gives a paper Trader live
// mark prices. The interface is small so tests can pass a fake without
// pulling in the full stream Manager.
type streamSource interface {
	Latest(symbol string) (stream.Update, bool)
	Subscribe() (<-chan stream.Update, func())
}

// symbolWatcher mirrors the optional capability on the stream backend; we
// use it here to avoid importing the concrete BybitLinearBackend type and
// keep the dependency surface small.
type symbolWatcher interface {
	Watch(symbols ...string)
}

// LiveTickerOptions controls the StartLiveTicker goroutine.
type LiveTickerOptions struct {
	// Source supplies live mark prices. If nil, StartLiveTicker is a no-op
	// and paper falls back to its REST-based MarkPriceFunc per cycle.
	Source streamSource
	// Watcher, if non-nil, is called with the union of all symbols paper
	// cares about (open positions + pending stop/TP orders) whenever the
	// set changes. Per-symbol subscription backends like Bybit need this;
	// global-stream backends like Binance ignore it.
	Watcher symbolWatcher
}

// StartLiveTicker spawns a background goroutine that:
//   - subscribes to the stream
//   - keeps the backend's symbol-watch list in sync with the trader's open
//     positions and pending stop/TP orders
//   - on every received tick relevant to a held symbol, runs tickLocked()
//     so SL/TP / liquidation / funding accrual fire sub-second instead of
//     waiting for the next 3-min decision cycle or a dashboard poll
//
// Calling StartLiveTicker more than once is a no-op; the goroutine lives
// until ctx is cancelled. When stream is nil, returns immediately.
func (t *Trader) StartLiveTicker(ctx context.Context, opts LiveTickerOptions) {
	if opts.Source == nil {
		return
	}
	t.mu.Lock()
	if t.liveTickerStarted {
		t.mu.Unlock()
		return
	}
	t.liveTickerStarted = true
	t.mu.Unlock()
	// Assign streamSource without the trader lock — see streamLatestMarkPrice
	// for why init-time-only writes need to stay lock-free. Concurrent reads
	// from goroutines started below see this assignment via the goroutine
	// happens-before edge.
	t.streamSource = opts.Source

	// Initial watch: subscribe to whatever symbols paper already holds.
	if opts.Watcher != nil {
		t.refreshWatchedSymbols(opts.Watcher)
	}

	updates, cancel := opts.Source.Subscribe()
	go t.runLiveTicker(ctx, updates, cancel, opts.Watcher)
}

// runLiveTicker is the goroutine body. It waits on either the stream channel
// (price updates that should trigger a tick) or a slow timer (so we still
// run liquidation/funding checks even when prices are flat). On every tick
// it also refreshes the watched-symbol set so newly-opened positions get
// subscribed promptly.
func (t *Trader) runLiveTicker(ctx context.Context, updates <-chan stream.Update, unsubscribe func(), watcher symbolWatcher) {
	defer unsubscribe()

	// Slow timer for the no-tick case (e.g. weekend on a low-volume symbol).
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	// Watch refresh runs at most once per second so we don't spam the
	// backend with subscribe messages on every received tick.
	var lastWatch time.Time
	maybeRefreshWatch := func() {
		if watcher == nil {
			return
		}
		now := t.now()
		if now.Sub(lastWatch) < time.Second {
			return
		}
		lastWatch = now
		t.refreshWatchedSymbols(watcher)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Infof("📄 [paper] live ticker stopped (context cancelled)")
			return

		case u, ok := <-updates:
			if !ok {
				logger.Infof("📄 [paper] live ticker stopped (stream closed)")
				return
			}
			// Only do work for symbols we hold a position in or have an
			// open SL/TP order for. Other ticks are dropped cheaply.
			t.mu.RLock()
			_, hasPos := t.positions[u.Symbol]
			hasOrder := false
			for _, o := range t.orders {
				if o.symbol == u.Symbol {
					hasOrder = true
					break
				}
			}
			t.mu.RUnlock()
			if !hasPos && !hasOrder {
				continue
			}
			t.mu.Lock()
			t.tickLocked()
			t.mu.Unlock()
			maybeRefreshWatch()

		case <-heartbeat.C:
			t.mu.Lock()
			t.tickLocked()
			t.mu.Unlock()
			maybeRefreshWatch()
		}
	}
}

// refreshWatchedSymbols computes the current set of symbols this paper
// trader cares about (positions + pending orders) and asks the watcher to
// subscribe. Idempotent on the watcher side — backend dedupes existing
// subscriptions.
func (t *Trader) refreshWatchedSymbols(watcher symbolWatcher) {
	t.mu.RLock()
	seen := make(map[string]bool, len(t.positions)+len(t.orders))
	for sym := range t.positions {
		seen[sym] = true
	}
	for _, o := range t.orders {
		seen[o.symbol] = true
	}
	t.mu.RUnlock()

	if len(seen) == 0 {
		return
	}
	syms := make([]string, 0, len(seen))
	for s := range seen {
		syms = append(syms, s)
	}
	watcher.Watch(syms...)
}

// streamLatestMarkPrice consults the live stream first; on a cache miss it
// returns (0, false) so the caller can fall back to the REST mark price.
//
// Lock-free read: streamSource is set exactly once by StartLiveTicker (during
// init, before any concurrent readers exist) and never reassigned. Reading it
// from inside paths that already hold t.mu (tickLocked → settleLiquidations
// → getMarkPrice) would deadlock; reading without a lock is safe because
// of the init-only write contract.
func (t *Trader) streamLatestMarkPrice(symbol string) (float64, bool) {
	src := t.streamSource
	if src == nil {
		return 0, false
	}
	u, ok := src.Latest(symbol)
	if !ok || u.Mark <= 0 {
		return 0, false
	}
	// Stale-tick guard: if the stream hasn't updated this symbol in over
	// a minute we treat it as a miss — paper would rather pay for a fresh
	// REST hit than act on a frozen price.
	if t.now().Sub(u.Timestamp) > time.Minute {
		return 0, false
	}
	return u.Mark, true
}
