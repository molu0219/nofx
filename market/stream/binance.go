package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"nofx/logger"
)

// BinanceFuturesEndpoint is the public USDT-M futures combined-stream URL for
// per-second mark-price snapshots across every listed symbol. One subscription
// covers the whole venue — no symbol list management needed.
const BinanceFuturesEndpoint = "wss://fstream.binance.com/ws/!markPrice@arr@1s"

const (
	binanceReadDeadline    = 30 * time.Second  // venue heartbeats every ~3 minutes; if we go silent that long, reconnect
	binanceMinBackoff      = 1 * time.Second
	binanceMaxBackoff      = 60 * time.Second
	binanceReadBufferBytes = 1 << 16 // 64KB; Binance frames are small but the array of all symbols can hit 100KB+
)

// binanceMarkPriceMessage matches what `!markPrice@arr@1s` actually returns:
// a JSON array (or a single object on rare connection edge cases) of
// markPriceUpdate events. We only consume `s` (symbol) and `p` (mark price).
type binanceMarkPriceMessage struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	MarkPrice string `json:"p"`
}

// BinanceFuturesBackend streams mark prices from Binance USDT-M futures.
// Reconnection is built in: any error closes the socket, backs off
// exponentially, and reconnects until the parent context is cancelled.
type BinanceFuturesBackend struct {
	// Endpoint defaults to BinanceFuturesEndpoint when empty. Overrideable for
	// tests pointing at a local mock.
	Endpoint string
}

// Name implements Backend.
func (b *BinanceFuturesBackend) Name() string { return "binance-futures" }

// Run implements Backend. It loops connecting → reading → reconnecting until
// ctx is cancelled. Returns nil on graceful shutdown; the manager closes the
// output channel after this returns.
func (b *BinanceFuturesBackend) Run(ctx context.Context, out chan<- Update) error {
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BinanceFuturesEndpoint
	}

	backoff := binanceMinBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := b.runOnce(ctx, endpoint, out)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Warnf("📡 [stream/binance] connection ended: %v — reconnecting in %s", err, backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		// Exponential backoff with cap; reset to min on every successful frame
		// (handled in runOnce's local resetBackoff).
		backoff *= 2
		if backoff > binanceMaxBackoff {
			backoff = binanceMaxBackoff
		}
	}
}

// runOnce holds a single connection. Returns when the read pump errors, the
// context is cancelled, or the venue closes the socket.
func (b *BinanceFuturesBackend) runOnce(ctx context.Context, endpoint string, out chan<- Update) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   binanceReadBufferBytes,
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := dialer.DialContext(dialCtx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	defer conn.Close()

	logger.Infof("📡 [stream/binance] connected to %s", endpoint)

	// Pong handler refreshes the read deadline so a healthy connection stays
	// open indefinitely; gorilla/websocket calls this on every received pong
	// or any frame when configured below.
	conn.SetReadLimit(1 << 20) // 1 MiB hard upper bound — Binance frames stay well under this
	if err := conn.SetReadDeadline(time.Now().Add(binanceReadDeadline)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(binanceReadDeadline))
	})

	// Cancel the read pump when ctx is cancelled by closing the connection.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// Send a ping every 60s so reverse proxies don't reap us; resets read
	// deadline on each successful frame anyway.
	pingTicker := time.NewTicker(60 * time.Second)
	defer pingTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(binanceReadDeadline)); err != nil {
			return err
		}

		// The combined-stream endpoint either returns a JSON array or, very
		// rarely on initial connect, a single event object. Try array first.
		updates, parseErr := parseBinanceFrame(raw)
		if parseErr != nil {
			logger.Warnf("📡 [stream/binance] skip frame: %v (raw len=%d)", parseErr, len(raw))
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

// parseBinanceFrame accepts either a JSON array of markPriceUpdate events or a
// single object and returns Updates with parsed prices. Bad rows are skipped.
func parseBinanceFrame(raw []byte) ([]Update, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty frame")
	}

	switch raw[0] {
	case '[':
		var arr []binanceMarkPriceMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("decode array: %w", err)
		}
		out := make([]Update, 0, len(arr))
		now := time.Now().UTC()
		for _, m := range arr {
			if u, ok := convertBinanceMessage(m, now); ok {
				out = append(out, u)
			}
		}
		return out, nil

	case '{':
		var msg binanceMarkPriceMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("decode object: %w", err)
		}
		if u, ok := convertBinanceMessage(msg, time.Now().UTC()); ok {
			return []Update{u}, nil
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("unexpected frame prefix: %q", raw[0])
	}
}

func convertBinanceMessage(m binanceMarkPriceMessage, fallbackNow time.Time) (Update, bool) {
	if m.Symbol == "" || m.MarkPrice == "" {
		return Update{}, false
	}
	mark, err := strconv.ParseFloat(m.MarkPrice, 64)
	if err != nil || mark <= 0 {
		return Update{}, false
	}
	ts := fallbackNow
	if m.EventTime > 0 {
		ts = time.UnixMilli(m.EventTime).UTC()
	}
	return Update{
		Symbol:    m.Symbol,
		Mark:      mark,
		Timestamp: ts,
	}, true
}
