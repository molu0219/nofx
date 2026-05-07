// Package stream provides a shared websocket-based mark-price feed used by
// both paper traders (for instant SL/TP triggers + sub-second equity updates)
// and live exchange traders (for tighter fill detection between OrderSync
// polls). One backend connection per venue feeds an in-memory cache that any
// number of consumers can read or subscribe to.
//
// Why centralise: NOFX previously had no websocket layer. Every position
// query went through `market.Get` → REST `/klines` per call, capping paper
// SL/TP at dashboard-poll resolution (~3 s) and forcing live exchanges to
// rely on 30 s OrderSync. Sharing one ws connection across all traders gives
// us sub-second mark prices for free.
package stream

import (
	"context"
	"sync"
	"time"
)

// Update is a single mark-price tick from the upstream venue.
type Update struct {
	Symbol    string    `json:"symbol"`
	Mark      float64   `json:"mark"`
	Timestamp time.Time `json:"timestamp"`
}

// Backend is what physically owns a connection to one venue. Implementations
// (Binance, Bybit, OKX, …) push every received update onto out and only return
// when ctx is cancelled. They're expected to handle reconnection internally —
// the manager treats them as long-running streams.
type Backend interface {
	// Name identifies this backend in logs (e.g. "binance-futures").
	Name() string
	// Run blocks until ctx is cancelled. Updates are sent on out; the manager
	// closes out after Run returns. Backends should retry transient errors
	// internally rather than returning them.
	Run(ctx context.Context, out chan<- Update) error
}

// Manager owns the single backend goroutine for a venue and fans price ticks
// out to the in-memory cache and to any subscribers. Designed as a singleton
// per venue but the type is plain so tests can construct isolated instances.
type Manager struct {
	backend Backend

	mu      sync.RWMutex
	prices  map[string]Update // most recent tick per symbol
	started bool
	cancel  context.CancelFunc

	subsMu sync.RWMutex
	subs   []*subscription
}

type subscription struct {
	ch     chan Update
	closed bool
	mu     sync.Mutex
}

// New constructs a Manager wired to the given backend. The manager is idle
// until Start is called.
func New(backend Backend) *Manager {
	return &Manager{
		backend: backend,
		prices:  make(map[string]Update),
	}
}

// Start launches the backend goroutine. Safe to call multiple times — the
// second and later calls are no-ops. Panics if backend is nil.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	derivedCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.mu.Unlock()

	out := make(chan Update, 256)
	go m.fanout(out)
	go func() {
		_ = m.backend.Run(derivedCtx, out)
		close(out)
	}()
}

// Stop cancels the backend goroutine and is safe to call before Start.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.started = false
}

// Latest returns the most recent tick for symbol or (zero, false) if the
// stream has not yet seen this symbol. ok=false is the "not yet warm" signal
// for callers that want to fall back to REST.
func (m *Manager) Latest(symbol string) (Update, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.prices[symbol]
	return u, ok
}

// Subscribe returns a channel that receives every update plus a cancel
// function. The channel is buffered; if the consumer is slow we drop the
// oldest update rather than blocking the backend (paper trader cares about
// "latest" not "every"). Cancel must be called when done to release resources.
func (m *Manager) Subscribe() (<-chan Update, func()) {
	sub := &subscription{ch: make(chan Update, 64)}
	m.subsMu.Lock()
	m.subs = append(m.subs, sub)
	m.subsMu.Unlock()
	return sub.ch, func() { m.unsubscribe(sub) }
}

func (m *Manager) unsubscribe(target *subscription) {
	m.subsMu.Lock()
	for i, sub := range m.subs {
		if sub == target {
			m.subs = append(m.subs[:i], m.subs[i+1:]...)
			break
		}
	}
	m.subsMu.Unlock()
	target.mu.Lock()
	if !target.closed {
		target.closed = true
		close(target.ch)
	}
	target.mu.Unlock()
}

// fanout owns the in-memory cache update + subscription dispatch. Running
// here (instead of inline in Run) keeps backend implementations simple.
func (m *Manager) fanout(in <-chan Update) {
	for u := range in {
		m.mu.Lock()
		m.prices[u.Symbol] = u
		m.mu.Unlock()

		m.subsMu.RLock()
		subs := m.subs
		m.subsMu.RUnlock()

		for _, sub := range subs {
			// Non-blocking send; on full buffer drop the oldest tick so the
			// consumer always sees the freshest mark.
			select {
			case sub.ch <- u:
			default:
				select {
				case <-sub.ch: // drain one stale
				default:
				}
				select {
				case sub.ch <- u:
				default:
				}
			}
		}
	}
}
