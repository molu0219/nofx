package trader

import (
	"sync"
	"testing"
	"time"
)

// newPauseTestTrader builds the minimal AutoTrader required to exercise
// maybeAutoPause. Full construction (NewAutoTrader) needs a real exchange,
// strategy engine, and store — too heavy for testing the threshold logic in
// isolation. The pause helper only touches isRunning, autoPaused, pauseReason,
// store, userID, and consecutiveAIFailures, so we wire just those.
func newPauseTestTrader(threshold int) *AutoTrader {
	return &AutoTrader{
		id:     "test-trader",
		name:   "test",
		userID: "user-1",
		config: AutoTraderConfig{
			MaxConsecutiveAIFailures: threshold,
		},
		isRunning: true,
	}
}

func TestMaybeAutoPause_BelowThresholdNoOp(t *testing.T) {
	at := newPauseTestTrader(5)
	at.consecutiveAIFailures = 4

	if at.maybeAutoPause("not yet") {
		t.Fatalf("should not pause at 4 < threshold 5")
	}
	if at.IsAutoPaused() {
		t.Fatalf("autoPaused unexpectedly true")
	}
	if !at.isRunning {
		t.Fatalf("isRunning was flipped before threshold hit")
	}
}

func TestMaybeAutoPause_TripsAtThreshold(t *testing.T) {
	at := newPauseTestTrader(5)
	at.consecutiveAIFailures = 5

	if !at.maybeAutoPause("threshold reached") {
		t.Fatalf("should pause when counter == threshold")
	}
	if !at.IsAutoPaused() {
		t.Fatalf("autoPaused not flipped")
	}
	if at.isRunning {
		t.Fatalf("isRunning not flipped to false")
	}
	if at.AutoPauseReason() != "threshold reached" {
		t.Fatalf("pauseReason got=%q", at.AutoPauseReason())
	}
}

func TestMaybeAutoPause_Idempotent(t *testing.T) {
	at := newPauseTestTrader(3)
	at.consecutiveAIFailures = 5

	if !at.maybeAutoPause("first call") {
		t.Fatalf("first call should pause")
	}
	if at.maybeAutoPause("second call") {
		t.Fatalf("second call should be a no-op once already paused")
	}
	// Original reason preserved — second reason ignored.
	if at.AutoPauseReason() != "first call" {
		t.Fatalf("pauseReason mutated by repeat call: %q", at.AutoPauseReason())
	}
}

func TestMaybeAutoPause_DefaultThreshold(t *testing.T) {
	// With config threshold = 0, the helper falls back to the package default.
	at := newPauseTestTrader(0)
	at.consecutiveAIFailures = DefaultMaxConsecutiveAIFailures - 1
	if at.maybeAutoPause("almost") {
		t.Fatalf("should not pause one short of default threshold")
	}
	at.consecutiveAIFailures = DefaultMaxConsecutiveAIFailures
	if !at.maybeAutoPause("at default threshold") {
		t.Fatalf("should pause at default threshold")
	}
}

// recordingNotifier captures every NotifyAutoPause call so tests can assert
// the right traderID + reason were forwarded.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []autoPauseNotice
}

type autoPauseNotice struct {
	traderID, traderName, reason string
}

func (r *recordingNotifier) NotifyAutoPause(traderID, traderName, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, autoPauseNotice{traderID, traderName, reason})
}

func (r *recordingNotifier) drained(t *testing.T, want int) []autoPauseNotice {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	for {
		r.mu.Lock()
		got := len(r.calls)
		r.mu.Unlock()
		if got >= want {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("notifier never received %d call(s); got %d", want, got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	r.mu.Lock()
	out := append([]autoPauseNotice(nil), r.calls...)
	r.mu.Unlock()
	return out
}

func TestMaybeAutoPause_FiresNotifierOnce(t *testing.T) {
	at := newPauseTestTrader(3)
	at.consecutiveAIFailures = 5
	rec := &recordingNotifier{}
	at.SetNotifier(rec)

	if !at.maybeAutoPause("AI failed 5 times") {
		t.Fatalf("first call should pause")
	}
	if at.maybeAutoPause("repeat") {
		t.Fatalf("repeat call should be no-op")
	}

	calls := rec.drained(t, 1)
	if len(calls) != 1 {
		t.Fatalf("notifier called %d times, want exactly 1", len(calls))
	}
	got := calls[0]
	if got.traderID != "test-trader" || got.traderName != "test" || got.reason != "AI failed 5 times" {
		t.Fatalf("unexpected notification: %+v", got)
	}
}

func TestMaybeAutoPause_NoNotifierIsNoopSafe(t *testing.T) {
	at := newPauseTestTrader(3)
	at.consecutiveAIFailures = 5
	// Don't call SetNotifier — must not panic.
	if !at.maybeAutoPause("ok no notifier") {
		t.Fatalf("expected pause")
	}
}

func TestAutoPauseReason_DistinguishesUserStop(t *testing.T) {
	// User-initiated stops leave autoPaused=false → AutoPauseReason returns "".
	at := newPauseTestTrader(5)
	at.isRunning = false // simulating a user Stop()

	if at.IsAutoPaused() {
		t.Fatalf("user-initiated stop should not set autoPaused")
	}
	if at.AutoPauseReason() != "" {
		t.Fatalf("AutoPauseReason should be empty for user stops, got %q", at.AutoPauseReason())
	}
}
