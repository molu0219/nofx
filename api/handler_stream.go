package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"nofx/auth"
	"nofx/logger"
	"nofx/market/stream"
)

// streamThrottle bounds how often we forward price-change ticks to the
// dashboard. Bybit's upstream pushes ~5-10 deltas/sec per symbol, so the
// effective ceiling is ~10 fps regardless of this value; we keep a 50ms
// floor to avoid pathological tight loops while still letting every real
// upstream update through.
//
// A heartbeat ticker forces a snapshot every heartbeatInterval even when
// the market is silent (e.g. low-volume coin between trades).
const (
	streamThrottle    = 50 * time.Millisecond
	heartbeatInterval = 2 * time.Second
	streamMaxRuntime  = 30 * time.Minute // SSE connection caps at 30min; client reconnects
)

// streamTickPayload is the per-event JSON the dashboard consumes. Mirrors what
// /api/account + /api/positions return so the frontend can swap polling for
// the stream without restructuring its state.
//
// Every numeric field the dashboard renders for "live" values must be on
// here; the frontend cache patch picks them up directly without computing
// derivations on its own. Anything we miss here stays at the stale SWR-poll
// value until the next 15s refresh, which is what was making total_pnl
// look frozen on the previous revision.
type streamTickPayload struct {
	TraderID         string                   `json:"trader_id"`
	Timestamp        time.Time                `json:"timestamp"`
	Equity           float64                  `json:"equity"`
	WalletBalance    float64                  `json:"wallet_balance"`
	AvailableBalance float64                  `json:"available_balance"`
	UnrealizedPnL    float64                  `json:"unrealized_pnl"`
	MarginUsed       float64                  `json:"margin_used"`
	MarginUsedPct    float64                  `json:"margin_used_pct"`
	TotalPnL         float64                  `json:"total_pnl"`
	TotalPnLPct      float64                  `json:"total_pnl_pct"`
	PositionCount    int                      `json:"position_count"`
	Positions        []map[string]interface{} `json:"positions"`
}

// handleStreamTrader serves a Server-Sent Events feed of equity + position
// snapshots for one trader. Updates fire whenever a relevant mark price ticks
// (subject to streamThrottle) or every heartbeatInterval as a fallback.
//
// Auth: SSE can't carry custom headers via the EventSource API, so we accept
// the JWT either via the standard Authorization header (when proxied) or via
// a `?token=` query param (when the dashboard opens the EventSource directly).
// Validated through the same auth.ValidateJWT path the middleware uses.
func (s *Server) handleStreamTrader(c *gin.Context) {
	traderID := c.Param("id")
	if traderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trader id is required"})
		return
	}

	// EventSource auth: prefer Authorization header, fall back to ?token=.
	tokenString := extractStreamToken(c)
	if tokenString == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token (use Authorization header or ?token=)"})
		return
	}
	claims, err := auth.ValidateJWT(tokenString)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return
	}
	userID := claims.UserID

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trader not found"})
		return
	}
	_ = userID // userID could gate access if traders ever became multi-tenant; for now ownership is implicit

	// SSE headers — flush them before the loop so the client sees the open.
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache, no-transform")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering
	c.Writer.WriteHeader(http.StatusOK)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported by underlying http writer"})
		return
	}
	flusher.Flush()

	// Send one initial snapshot so the client sees data without waiting for
	// the first market tick.
	if snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader); err == nil {
		writeSSEEvent(c.Writer, flusher, "tick", snap)
	}

	sub, unsubscribe := stream.Default().Subscribe()
	defer unsubscribe()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	hardLimit := time.After(streamMaxRuntime)

	ctx := c.Request.Context()
	lastEmit := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hardLimit:
			// Encourage the EventSource client to reconnect via a closing
			// "retry" hint; SSE auto-reconnect handles the rest.
			fmt.Fprintf(c.Writer, "retry: 1000\nevent: close\ndata: {\"reason\":\"max_runtime\"}\n\n")
			flusher.Flush()
			return

		case <-sub:
			// Coalesce bursts: if we sent within the throttle window, drop this
			// tick. The cache holds the latest mark, so the next sub or
			// heartbeat will pick it up with zero data loss.
			if time.Since(lastEmit) < streamThrottle {
				continue
			}
			lastEmit = time.Now()
			snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader)
			if err != nil {
				continue
			}
			if !writeSSEEvent(c.Writer, flusher, "tick", snap) {
				return
			}

		case <-heartbeat.C:
			lastEmit = time.Now()
			snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader)
			if err != nil {
				continue
			}
			if !writeSSEEvent(c.Writer, flusher, "tick", snap) {
				return
			}
		}
	}
}

// snapshotSource is the AutoTrader-side surface buildTraderSnapshot needs.
// We use the AutoTrader-level transforming methods (GetAccountInfo,
// GetPositions) — NOT the underlying Trader interface — because those return
// snake_case fields the dashboard already consumes via /api/account and
// /api/positions. Mirroring the same transform lets the SSE payload drop
// straight into the SWR cache without per-component shape coercion.
type snapshotSource interface {
	GetAccountInfo() (map[string]interface{}, error)
	GetPositions() ([]map[string]interface{}, error)
}

// buildTraderSnapshot pulls the current account + position state from the
// trader and packages it for SSE delivery. Underlying GetBalance /
// GetPositions read the live mark-price cache, so the snapshot is always
// sub-second-fresh on the paper exchange.
func buildTraderSnapshot(traderID string, initialBalance float64, t snapshotSource) (streamTickPayload, error) {
	acct, err := t.GetAccountInfo()
	if err != nil {
		return streamTickPayload{}, err
	}
	positions, err := t.GetPositions()
	if err != nil {
		// Still emit a balance-only snapshot rather than killing the stream.
		positions = nil
	}

	equity := readFloat(acct, "total_equity")
	wallet := readFloat(acct, "wallet_balance")
	available := readFloat(acct, "available_balance")
	unrealized := readFloat(acct, "unrealized_profit")
	margin := readFloat(acct, "margin_used")
	marginPct := readFloat(acct, "margin_used_pct")
	totalPnL := readFloat(acct, "total_pnl")
	pnlPct := readFloat(acct, "total_pnl_pct")
	if pnlPct == 0 && initialBalance > 0 {
		pnlPct = ((equity - initialBalance) / initialBalance) * 100
	}
	if totalPnL == 0 && initialBalance > 0 {
		totalPnL = equity - initialBalance
	}

	// Sort positions deterministically by opening notional (entry_price × qty)
	// descending. Without this the dashboard list reshuffles on every tick
	// because Go's map iteration randomises the underlying order — visually
	// jarring and impossible to track which position is which.
	sort.SliceStable(positions, func(i, j int) bool {
		return openingNotional(positions[i]) > openingNotional(positions[j])
	})

	return streamTickPayload{
		TraderID:         traderID,
		Timestamp:        time.Now().UTC(),
		Equity:           equity,
		WalletBalance:    wallet,
		AvailableBalance: available,
		UnrealizedPnL:    unrealized,
		MarginUsed:       margin,
		MarginUsedPct:    marginPct,
		TotalPnL:         totalPnL,
		TotalPnLPct:      pnlPct,
		PositionCount:    len(positions),
		Positions:        positions,
	}, nil
}

// openingNotional pulls entry_price × quantity from a position record. Falls
// back to 0 when fields are missing so the comparator stays well-defined.
func openingNotional(pos map[string]interface{}) float64 {
	entry, _ := pos["entry_price"].(float64)
	qty, _ := pos["quantity"].(float64)
	if qty < 0 {
		qty = -qty
	}
	return entry * qty
}

// readFloat picks the first numeric value among the given keys.
func readFloat(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int:
				return float64(x)
			case int64:
				return float64(x)
			}
		}
	}
	return 0
}

// writeSSEEvent serialises payload to JSON and writes it as a single SSE
// event of the given type. Returns false on write error so the caller can
// exit the loop (typically client disconnect).
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, eventName string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf("📡 [stream/sse] marshal: %v", err)
		return true // soft fail; keep the stream alive
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, body); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// extractStreamToken pulls the JWT from either Authorization: Bearer ... or
// from a ?token=... query parameter (EventSource fallback).
func extractStreamToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && h[:len(prefix)] == prefix {
			return h[len(prefix):]
		}
	}
	return c.Query("token")
}
