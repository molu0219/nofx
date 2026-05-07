package trader

// Notifier is the optional out-of-band channel auto-pause and other
// operator-attention events can use to reach the user without the dashboard.
// Concrete implementations live next to whichever transport they use
// (telegram, email, webhook, …); the trader package only depends on this
// tiny interface to avoid pulling those packages into its build graph.
//
// Methods are best-effort and must never block the trading loop. Errors are
// surfaced to the implementation (it logs them); the trader treats every
// notification as fire-and-forget.
type Notifier interface {
	// NotifyAutoPause is called once when a trader transitions itself out of
	// running state due to a runtime fault (currently: N consecutive AI
	// failures). reason is a short human-readable phrase ready for display.
	NotifyAutoPause(traderID, traderName, reason string)
}

// noopNotifier is the default when no concrete notifier is wired. Keeps the
// call sites from needing nil checks while still costing nothing at runtime.
type noopNotifier struct{}

func (noopNotifier) NotifyAutoPause(_, _, _ string) {}
