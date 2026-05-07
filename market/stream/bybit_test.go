package stream

import (
	"encoding/json"
	"testing"
)

func TestParseBybitFrame_Snapshot(t *testing.T) {
	raw := []byte(`{"topic":"tickers.BTCUSDT","type":"snapshot","ts":1700000000000,"data":{"symbol":"BTCUSDT","markPrice":"42000.5","lastPrice":"42001.0","indexPrice":"42000.0"}}`)
	updates, err := parseBybitTickerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 1 || updates[0].Symbol != "BTCUSDT" || updates[0].Mark != 42000.5 {
		t.Fatalf("got %+v", updates)
	}
}

func TestParseBybitFrame_FallsBackToLastPrice(t *testing.T) {
	// Delta with markPrice missing → use lastPrice as the next-best mark proxy.
	raw := []byte(`{"topic":"tickers.ETHUSDT","type":"delta","ts":1700000000000,"data":{"symbol":"ETHUSDT","lastPrice":"2200.0"}}`)
	updates, err := parseBybitTickerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 1 || updates[0].Mark != 2200.0 {
		t.Fatalf("got %+v", updates)
	}
}

func TestParseBybitFrame_IgnoresSubscribeAck(t *testing.T) {
	raw := []byte(`{"success":true,"ret_msg":"","op":"subscribe","conn_id":"abc"}`)
	updates, err := parseBybitTickerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("ack should produce no updates, got %+v", updates)
	}
}

func TestParseBybitFrame_IgnoresPriceless(t *testing.T) {
	// A delta with no useful price field — must be silently skipped.
	raw := []byte(`{"topic":"tickers.BTCUSDT","type":"delta","ts":1,"data":{"symbol":"BTCUSDT","volume24h":"100"}}`)
	updates, err := parseBybitTickerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("got %+v", updates)
	}
}

// Sanity: envelope decodes cleanly.
func TestBybitTickerFrame_DecodableEnvelope(t *testing.T) {
	raw := []byte(`{"topic":"tickers.BTCUSDT","type":"snapshot","ts":1700000000000,"data":{"symbol":"BTCUSDT","markPrice":"42000","lastPrice":"42001"}}`)
	var f bybitTickerFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Topic != "tickers.BTCUSDT" || f.Data.Symbol != "BTCUSDT" {
		t.Fatalf("envelope mismatch: %+v", f)
	}
}

func TestBybitWatch_DedupesAndReturnsEarlyBeforeRun(t *testing.T) {
	b := NewBybitLinearBackend("BTCUSDT")
	// Adding the same symbol twice + a new one — should track exactly two unique entries.
	b.Watch("BTCUSDT", "ETHUSDT")
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.symbols) != 2 || !b.symbols["BTCUSDT"] || !b.symbols["ETHUSDT"] {
		t.Fatalf("watched set got=%v", b.symbols)
	}
}
