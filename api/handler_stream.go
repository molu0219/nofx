package api

import (
	"encoding/json"
	"fmt"
	"net/http"
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
type streamTickPayload struct {
	TraderID         string                   `json:"trader_id"`
	Timestamp        time.Time                `json:"timestamp"`
	Equity           float64                  `json:"equity"`
	WalletBalance    float64                  `json:"wallet_balance"`
	AvailableBalance float64                  `json:"available_balance"`
	UnrealizedPnL    float64                  `json:"unrealized_pnl"`
	MarginUsed       float64                  `json:"margin_used"`
	TotalPnLPct      float64                  `json:"total_pnl_pct"`
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
	if snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader.GetUnderlyingTrader()); err == nil {
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
			snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader.GetUnderlyingTrader())
			if err != nil {
				continue
			}
			if !writeSSEEvent(c.Writer, flusher, "tick", snap) {
				return
			}

		case <-heartbeat.C:
			lastEmit = time.Now()
			snap, err := buildTraderSnapshot(traderID, trader.GetInitialBalance(), trader.GetUnderlyingTrader())
			if err != nil {
				continue
			}
			if !writeSSEEvent(c.Writer, flusher, "tick", snap) {
				return
			}
		}
	}
}

// snapshotSource is the minimal Trader-side surface buildTraderSnapshot needs.
// Inlining the interface here keeps handler_stream from importing the trader
// package (which would create a cycle: api → trader → store … → api).
type snapshotSource interface {
	GetBalance() (map[string]interface{}, error)
	GetPositions() ([]map[string]interface{}, error)
}

// buildTraderSnapshot pulls the current account + position state from the
// trader and packages it for SSE delivery. The trader's own GetBalance /
// GetPositions reach into the live mark-price cache, so the snapshot is
// always sub-second-fresh on the paper exchange.
func buildTraderSnapshot(traderID string, initialBalance float64, t snapshotSource) (streamTickPayload, error) {
	bal, err := t.GetBalance()
	if err != nil {
		return streamTickPayload{}, err
	}
	positions, err := t.GetPositions()
	if err != nil {
		// Still emit a balance-only snapshot rather than killing the stream.
		positions = nil
	}

	wallet := readFloat(bal, "totalWalletBalance", "wallet_balance", "balance")
	equity := readFloat(bal, "totalEquity", "total_equity")
	if equity == 0 {
		// Fallback when the trader didn't compute equity itself.
		equity = wallet + readFloat(bal, "totalUnrealizedProfit", "unrealizedPnL")
	}
	available := readFloat(bal, "availableBalance", "available_balance")
	unrealized := readFloat(bal, "totalUnrealizedProfit", "unrealizedPnL", "unrealized_pnl")
	margin := readFloat(bal, "marginUsed", "margin_used")

	pnlPct := 0.0
	if initialBalance > 0 {
		pnlPct = ((equity - initialBalance) / initialBalance) * 100
	}

	return streamTickPayload{
		TraderID:         traderID,
		Timestamp:        time.Now().UTC(),
		Equity:           equity,
		WalletBalance:    wallet,
		AvailableBalance: available,
		UnrealizedPnL:    unrealized,
		MarginUsed:       margin,
		TotalPnLPct:      pnlPct,
		Positions:        positions,
	}, nil
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
