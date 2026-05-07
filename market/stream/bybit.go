package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"nofx/logger"
)

// BybitLinearEndpoint is the public USDT-perp (linear) websocket endpoint.
// Unlike Binance's `!markPrice@arr`, Bybit requires per-symbol subscription —
// we maintain that subscription set dynamically as paper trader symbols change.
const BybitLinearEndpoint = "wss://stream.bybit.com/v5/public/linear"

// bybitTickerFrame matches the public ticker stream envelope. Both snapshot
// and delta variants share this shape; deltas may omit fields, so all
// numeric fields are optional strings parsed lazily.
type bybitTickerFrame struct {
	Topic string `json:"topic"`
	Type  string `json:"type"`
	Ts    int64  `json:"ts"`
	Data  struct {
		Symbol     string `json:"symbol"`
		MarkPrice  string `json:"markPrice,omitempty"`
		LastPrice  string `json:"lastPrice,omitempty"`
		IndexPrice string `json:"indexPrice,omitempty"`
	} `json:"data"`

	// Subscribe acknowledgement fields — present on op=subscribe responses.
	Success bool   `json:"success,omitempty"`
	Op      string `json:"op,omitempty"`
}

// BybitLinearBackend streams mark prices from Bybit USDT perpetuals. Useful
// when Binance Futures WS is unreachable (regional restrictions). Symbols
// must be set up-front via Watch before Run starts; calling Watch later
// triggers an extra subscribe message on the live connection.
type BybitLinearBackend struct {
	// Endpoint defaults to BybitLinearEndpoint when empty.
	Endpoint string

	mu      sync.Mutex
	symbols map[string]bool // currently-subscribed set
	conn    *websocket.Conn // protected by mu; nil when disconnected
}

// NewBybitLinearBackend pre-registers the given symbols. Symbols can also be
// added later via Watch(); subscriptions are maintained across reconnects.
func NewBybitLinearBackend(symbols ...string) *BybitLinearBackend {
	b := &BybitLinearBackend{symbols: make(map[string]bool)}
	for _, s := range symbols {
		b.symbols[s] = true
	}
	return b
}

// Name implements Backend.
func (b *BybitLinearBackend) Name() string { return "bybit-linear" }

// Watch adds symbols to the subscription set. Safe to call before or after
// Run; if a connection is live, a subscribe message is dispatched immediately.
func (b *BybitLinearBackend) Watch(symbols ...string) {
	b.mu.Lock()
	added := make([]string, 0, len(symbols))
	for _, s := range symbols {
		if !b.symbols[s] {
			b.symbols[s] = true
			added = append(added, s)
		}
	}
	conn := b.conn
	b.mu.Unlock()

	if conn != nil && len(added) > 0 {
		_ = sendBybitSubscribe(conn, added)
	}
}

// Run implements Backend with auto-reconnect.
func (b *BybitLinearBackend) Run(ctx context.Context, out chan<- Update) error {
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BybitLinearEndpoint
	}

	backoff := 1 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := b.runOnce(ctx, endpoint, out)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Warnf("📡 [stream/bybit] connection ended: %v — reconnecting in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (b *BybitLinearBackend) runOnce(ctx context.Context, endpoint string, out chan<- Update) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := dialer.DialContext(dialCtx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	logger.Infof("📡 [stream/bybit] connected to %s", endpoint)

	b.mu.Lock()
	b.conn = conn
	syms := make([]string, 0, len(b.symbols))
	for s := range b.symbols {
		syms = append(syms, s)
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.conn = nil
		b.mu.Unlock()
		_ = conn.Close()
	}()

	if len(syms) > 0 {
		if err := sendBybitSubscribe(conn, syms); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}

	const readDeadline = 30 * time.Second
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readDeadline))
	})
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	pingTicker := time.NewTicker(20 * time.Second) // Bybit recommends ping every 20s
	defer pingTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				_ = conn.WriteJSON(map[string]any{"op": "ping"})
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))

		updates, parseErr := parseBybitTickerFrame(raw)
		if parseErr != nil {
			logger.Warnf("📡 [stream/bybit] skip frame: %v", parseErr)
			continue
		}
		for _, u := range updates {
			select {
			case <-ctx.Done():
				return nil
			case out <- u:
			}
		}
	}
}

// sendBybitSubscribe issues a single op=subscribe with topics for every
// symbol in syms. Bybit accepts up to ~10 topics per subscribe message;
// chunking keeps us well under any per-frame limit.
func sendBybitSubscribe(conn *websocket.Conn, syms []string) error {
	const chunk = 10
	for i := 0; i < len(syms); i += chunk {
		end := i + chunk
		if end > len(syms) {
			end = len(syms)
		}
		topics := make([]string, 0, end-i)
		for _, s := range syms[i:end] {
			topics = append(topics, "tickers."+s)
		}
		if err := conn.WriteJSON(map[string]any{
			"op":   "subscribe",
			"args": topics,
		}); err != nil {
			return err
		}
	}
	return nil
}

// parseBybitTickerFrame extracts an Update (or zero, for ack frames) from a
// raw ticker websocket message. Mark price is preferred; falls back to last
// price when only the latter is present (delta updates often omit markPrice).
func parseBybitTickerFrame(raw []byte) ([]Update, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	var f bybitTickerFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	// Subscribe-ack and pong frames have no `topic` and no `data.symbol`. Skip.
	if f.Op != "" || f.Topic == "" || f.Data.Symbol == "" {
		return nil, nil
	}

	// Prefer markPrice; fall back to lastPrice; final fallback to indexPrice.
	priceStr := f.Data.MarkPrice
	if priceStr == "" {
		priceStr = f.Data.LastPrice
	}
	if priceStr == "" {
		priceStr = f.Data.IndexPrice
	}
	if priceStr == "" {
		// Delta with no price-relevant fields — skip silently.
		return nil, nil
	}

	mark, err := strconv.ParseFloat(priceStr, 64)
	if err != nil || mark <= 0 {
		return nil, nil
	}

	ts := time.Now().UTC()
	if f.Ts > 0 {
		ts = time.UnixMilli(f.Ts).UTC()
	}
	return []Update{{
		Symbol:    f.Data.Symbol,
		Mark:      mark,
		Timestamp: ts,
	}}, nil
}
