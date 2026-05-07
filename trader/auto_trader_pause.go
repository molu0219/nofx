package trader

import (
	"nofx/logger"
)

// DefaultMaxConsecutiveAIFailures is the failure-streak threshold at which
// the trader stops trying entirely. Safe mode (no new positions) kicks in at
// 3 failures; full auto-pause is two layers above that — by the time we hit
// 10 consecutive failures the AI provider, network, or wallet config is
// almost certainly broken and we should stop burning quota until a human
// looks. SPEC F007-T05.
const DefaultMaxConsecutiveAIFailures = 10

// maybeAutoPause checks the consecutive-failure counter against the configured
// threshold and, if exceeded, transitions the trader to a paused state:
//   - flips isRunning = false so the main loop exits at next tick
//   - persists status=stopped in the trader store (when configured)
//   - records the human-readable reason on the AutoTrader
//
// Returns true iff the trader was just paused by this call (idempotent — calling
// again on an already-paused trader is a no-op).
//
// This is the only path that should set autoPaused=true; user-initiated Stop()
// leaves autoPaused=false so the dashboard can distinguish "user stopped" from
// "system stopped because something is broken".
func (at *AutoTrader) maybeAutoPause(reason string) bool {
	threshold := at.config.MaxConsecutiveAIFailures
	if threshold <= 0 {
		threshold = DefaultMaxConsecutiveAIFailures
	}
	if at.consecutiveAIFailures < threshold {
		return false
	}

	at.isRunningMutex.Lock()
	if at.autoPaused {
		at.isRunningMutex.Unlock()
		return false
	}
	at.autoPaused = true
	at.pauseReason = reason
	at.isRunning = false
	at.isRunningMutex.Unlock()

	at.logErrorf("⛔ AUTO-PAUSED after %d consecutive AI failures: %s",
		at.consecutiveAIFailures, reason)
	at.logErrorf("⛔ Trader %s (%s) stopped — fix the root cause then restart from the dashboard.",
		at.id, at.name)

	if at.store != nil && at.userID != "" {
		if err := at.store.Trader().UpdateStatus(at.userID, at.id, false); err != nil {
			logger.Warnf("⛔ failed to persist auto-pause status for trader %s: %v", at.id, err)
		}
	}
	return true
}
