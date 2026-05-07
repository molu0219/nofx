package stream

import (
	"context"
	"os"
	"sync"
)

// defaultMgr is the process-wide manager used by paper traders + any caller
// that wants the latest mark price without managing its own subscription.
// Lazily started on first use.
//
// The default backend is Bybit USDT linear because:
//   - Binance Futures WS (`fstream.binance.com`) is geo-restricted in many
//     networks where the REST kline endpoint still works (NOFX paper users
//     hit this in WSL, EU, parts of Asia).
//   - Bybit publishes per-symbol mark price at ~10/sec deltas, which is
//     plenty for SL/TP triggers.
//   - Symbol naming matches Binance USDT-M (BTCUSDT, ETHUSDT…), so paper
//     trader prompts and order recording stay consistent.
//
// Set NOFX_STREAM_BACKEND=binance to override.
var (
	defaultMu  sync.Mutex
	defaultMgr *Manager
)

// Default returns the lazily-initialised process-wide Manager. The first call
// also kicks off the websocket connection; subsequent calls reuse it.
func Default() *Manager {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultMgr == nil {
		var backend Backend = NewBybitLinearBackend()
		if os.Getenv("NOFX_STREAM_BACKEND") == "binance" {
			backend = &BinanceFuturesBackend{}
		}
		defaultMgr = New(backend)
		defaultMgr.Start(context.Background())
	}
	return defaultMgr
}

// Watch is a convenience over Default() for backends that need explicit
// per-symbol subscriptions (Bybit, OKX). Backends that subscribe globally
// (Binance `!markPrice@arr@1s`) ignore the call.
func Watch(symbols ...string) {
	mgr := Default()
	if w, ok := mgr.backend.(symbolWatcher); ok {
		w.Watch(symbols...)
	}
}

// symbolWatcher is the optional capability for backends with demand-driven
// subscription (per-symbol).
type symbolWatcher interface {
	Watch(symbols ...string)
}
