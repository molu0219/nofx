package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeBackend lets tests drive the manager with deterministic updates.
type fakeBackend struct {
	mu     sync.Mutex
	closed bool
	feed   []Update
	delay  time.Duration
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) Run(ctx context.Context, out chan<- Update) error {
	for _, u := range b.feed {
		if b.delay > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(b.delay):
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case out <- u:
		}
	}
	<-ctx.Done()
	return nil
}

func TestManager_LatestReflectsLastTick(t *testing.T) {
	mgr := New(&fakeBackend{
		feed: []Update{
			{Symbol: "BTCUSDT", Mark: 100, Timestamp: time.Unix(1, 0).UTC()},
			{Symbol: "BTCUSDT", Mark: 101, Timestamp: time.Unix(2, 0).UTC()},
			{Symbol: "ETHUSDT", Mark: 50, Timestamp: time.Unix(3, 0).UTC()},
		},
	})
	mgr.Start(context.Background())
	defer mgr.Stop()

	waitFor(t, func() bool {
		u, ok := mgr.Latest("BTCUSDT")
		return ok && u.Mark == 101
	}, "BTCUSDT mark to reach 101")

	if u, ok := mgr.Latest("ETHUSDT"); !ok || u.Mark != 50 {
		t.Fatalf("ETHUSDT got %+v", u)
	}
	if _, ok := mgr.Latest("MISSING"); ok {
		t.Fatalf("expected miss for unsubscribed symbol")
	}
}

func TestManager_SubscribeReceivesUpdates(t *testing.T) {
	mgr := New(&fakeBackend{
		feed: []Update{
			{Symbol: "BTCUSDT", Mark: 100, Timestamp: time.Unix(1, 0).UTC()},
			{Symbol: "BTCUSDT", Mark: 101, Timestamp: time.Unix(2, 0).UTC()},
		},
		delay: 5 * time.Millisecond,
	})
	ch, cancel := mgr.Subscribe()
	defer cancel()

	mgr.Start(context.Background())
	defer mgr.Stop()

	first, ok := receiveWithin(ch, time.Second)
	if !ok || first.Mark != 100 {
		t.Fatalf("first update got=%+v ok=%v", first, ok)
	}
	second, ok := receiveWithin(ch, time.Second)
	if !ok || second.Mark != 101 {
		t.Fatalf("second update got=%+v ok=%v", second, ok)
	}
}

func TestManager_SubscribeDropsStaleOnFullBuffer(t *testing.T) {
	// Fast producer with a slow consumer should drop intermediate ticks but
	// always end on the latest. We use a buffer of 64 in production, but
	// drowning it requires too many feed events — easier to verify via the
	// drop-oldest path inside fanout: the latest tick must be deliverable
	// even if we never read.
	feed := make([]Update, 200)
	for i := range feed {
		feed[i] = Update{Symbol: "BTCUSDT", Mark: float64(i), Timestamp: time.Now()}
	}
	mgr := New(&fakeBackend{feed: feed})
	ch, cancel := mgr.Subscribe()
	defer cancel()
	mgr.Start(context.Background())
	defer mgr.Stop()

	// Drain at our own pace; we don't claim every value, just that we
	// eventually see the highest.
	deadline := time.After(2 * time.Second)
	highest := -1.0
	for {
		select {
		case u := <-ch:
			if u.Mark > highest {
				highest = u.Mark
			}
			if highest == 199 {
				return
			}
		case <-deadline:
			t.Fatalf("never observed final tick (highest=%v)", highest)
		}
	}
}

func TestManager_StopReleasesBackend(t *testing.T) {
	mgr := New(&fakeBackend{feed: []Update{{Symbol: "BTCUSDT", Mark: 100}}})
	mgr.Start(context.Background())
	mgr.Stop()
	mgr.Stop() // idempotent
}

func TestParseBinanceFrame_Array(t *testing.T) {
	raw := []byte(`[{"e":"markPriceUpdate","E":1700000000000,"s":"BTCUSDT","p":"42000.5"},{"e":"markPriceUpdate","E":1700000000000,"s":"ETHUSDT","p":"2200.0"}]`)
	updates, err := parseBinanceFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	if updates[0].Symbol != "BTCUSDT" || updates[0].Mark != 42000.5 {
		t.Fatalf("update[0] got %+v", updates[0])
	}
	if updates[1].Symbol != "ETHUSDT" || updates[1].Mark != 2200.0 {
		t.Fatalf("update[1] got %+v", updates[1])
	}
}

func TestParseBinanceFrame_SkipsMalformedRows(t *testing.T) {
	// Empty symbol + zero price are dropped silently.
	raw := []byte(`[{"e":"markPriceUpdate","s":"","p":"0"},{"e":"markPriceUpdate","s":"BTCUSDT","p":"50"}]`)
	updates, err := parseBinanceFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 1 || updates[0].Symbol != "BTCUSDT" {
		t.Fatalf("got %+v", updates)
	}
}

func TestParseBinanceFrame_RejectsGarbage(t *testing.T) {
	if _, err := parseBinanceFrame([]byte("hello")); err == nil {
		t.Fatalf("expected error on non-JSON frame")
	}
	if _, err := parseBinanceFrame([]byte("")); err == nil {
		t.Fatalf("expected error on empty frame")
	}
}

// helpers

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for: %s", msg)
		case <-tick.C:
		}
	}
}

func receiveWithin(ch <-chan Update, d time.Duration) (Update, bool) {
	select {
	case u, ok := <-ch:
		return u, ok
	case <-time.After(d):
		return Update{}, false
	}
}
