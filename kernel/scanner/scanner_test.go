package scanner

import (
	"context"
	"testing"
	"time"
)

func mkScannerWithFakeTickers(t *testing.T, snapshots [][]TickerSnapshot, opts ...func(*Config)) *Scanner {
	t.Helper()
	calls := 0
	cfg := Config{
		Interval:      10 * time.Minute,
		WatchlistSize: 3,
		HistoryDepth:  6,
		MissThreshold: 2,
		FetchTickers: func(ctx context.Context) ([]TickerSnapshot, error) {
			i := calls
			calls++
			if i >= len(snapshots) {
				i = len(snapshots) - 1 // hold last snapshot if exceeded
			}
			return snapshots[i], nil
		},
		Scorer: NewRuleScorer(ScoringWeights{}),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return New(cfg)
}

func tk(sym string, price, vol float64) TickerSnapshot {
	return TickerSnapshot{Symbol: sym, Price: price, QuoteVolume24h: vol, PriceChange24hPct: 0}
}

func TestScanner_TopByVolumeFromSingleRefresh(t *testing.T) {
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{{
		tk("BTCUSDT", 100, 1_000_000),
		tk("ETHUSDT", 50, 500_000),
		tk("SOLUSDT", 10, 200_000),
		tk("LOWUSDT", 1, 1_000), // tiny volume
	}})
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := s.Watchlist()
	if len(got) != 3 {
		t.Fatalf("watchlist size got=%d want=3 (got=%v)", len(got), got)
	}
	// With pure volume, top 3 are BTC, ETH, SOL.
	if got[0] != "BTCUSDT" {
		t.Fatalf("top should be BTCUSDT, got %v", got)
	}
}

func TestScanner_DeltaComputedFromHistory(t *testing.T) {
	// First refresh: BTC=100. Second refresh: BTC=110 (+10%). The 10-min
	// delta should reflect the move.
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{
		{tk("BTCUSDT", 100, 1_000_000)},
		{tk("BTCUSDT", 110, 1_000_000)},
	})
	_ = s.Refresh(context.Background())
	_ = s.Refresh(context.Background())
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].PriceChange10m == 0 {
		t.Fatalf("expected non-zero 10m delta, got %+v", snap)
	}
	// (110 - 100) / 100 = 10
	if got := snap[0].PriceChange10m; got < 9.5 || got > 10.5 {
		t.Fatalf("PriceChange10m got=%.2f want≈10", got)
	}
}

func TestScanner_OpenPositionAlwaysProtected(t *testing.T) {
	// LOWUSDT has tiny volume — would never make top-3 on score. But it's
	// an open position so it MUST be in the watchlist.
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{{
		tk("BTCUSDT", 100, 1_000_000),
		tk("ETHUSDT", 50, 500_000),
		tk("SOLUSDT", 10, 200_000),
		tk("LOWUSDT", 1, 100),
	}}, func(cfg *Config) {
		cfg.GetOpenSymbols = func() []string { return []string{"LOWUSDT"} }
	})
	_ = s.Refresh(context.Background())
	got := s.Watchlist()
	found := false
	for _, sym := range got {
		if sym == "LOWUSDT" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("LOWUSDT (open position) missing from watchlist: %v", got)
	}
}

func TestScanner_HysteresisGracePeriod(t *testing.T) {
	// Refresh 1: BTC, ETH, SOL → watchlist = [BTC, ETH, SOL] (size=3)
	// Refresh 2: BTC, ETH, X      → SOL drops to 4th. With MissThreshold=2,
	//   SOL gets 1 grace round and stays.
	// Refresh 3: BTC, ETH, X      → SOL misses again, total 2 misses, drops out.
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{
		{
			tk("BTCUSDT", 100, 10_000_000),
			tk("ETHUSDT", 50, 5_000_000),
			tk("SOLUSDT", 10, 2_000_000),
			tk("XUSDT", 1, 100),
		},
		{
			tk("BTCUSDT", 100, 10_000_000),
			tk("ETHUSDT", 50, 5_000_000),
			tk("XUSDT", 1, 9_000_000),    // X now beats SOL
			tk("SOLUSDT", 10, 100),       // SOL drops to last
		},
		{
			tk("BTCUSDT", 100, 10_000_000),
			tk("ETHUSDT", 50, 5_000_000),
			tk("XUSDT", 1, 9_000_000),
			tk("SOLUSDT", 10, 100),
		},
	}, func(cfg *Config) { cfg.MissThreshold = 2 })

	_ = s.Refresh(context.Background())
	if !contains(s.Watchlist(), "SOLUSDT") {
		t.Fatalf("after 1st: SOL should be in watchlist: %v", s.Watchlist())
	}
	_ = s.Refresh(context.Background())
	if !contains(s.Watchlist(), "SOLUSDT") {
		t.Fatalf("after 2nd (1 miss, threshold=2): SOL should stay: %v", s.Watchlist())
	}
	_ = s.Refresh(context.Background())
	if contains(s.Watchlist(), "SOLUSDT") {
		t.Fatalf("after 3rd (2nd miss): SOL should be dropped: %v", s.Watchlist())
	}
}

func TestScanner_SubscribeFiresOnChange(t *testing.T) {
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{
		{tk("BTCUSDT", 100, 1_000_000), tk("ETHUSDT", 50, 500_000), tk("SOLUSDT", 10, 200_000)},
		{tk("BTCUSDT", 100, 1_000_000), tk("ETHUSDT", 50, 500_000), tk("XUSDT", 1, 9_000_000)},
	})
	ch, cancel := s.Subscribe()
	defer cancel()
	_ = s.Refresh(context.Background())
	// First refresh changes from empty → expect notification.
	select {
	case got := <-ch:
		if len(got) != 3 {
			t.Fatalf("first sub got len=%d", len(got))
		}
	case <-time.After(time.Second):
		t.Fatalf("no notification after first refresh")
	}
	_ = s.Refresh(context.Background())
	// Second refresh changes set (SOL → X) → expect notification.
	select {
	case got := <-ch:
		if !contains(got, "XUSDT") {
			t.Fatalf("expected X in update: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("no notification after second refresh")
	}
}

func TestScanner_NoChangeNoNotification(t *testing.T) {
	feed := []TickerSnapshot{tk("BTCUSDT", 100, 1_000_000), tk("ETHUSDT", 50, 500_000), tk("SOLUSDT", 10, 200_000)}
	s := mkScannerWithFakeTickers(t, [][]TickerSnapshot{feed, feed})
	ch, cancel := s.Subscribe()
	defer cancel()
	_ = s.Refresh(context.Background())
	<-ch // drain initial
	_ = s.Refresh(context.Background())
	select {
	case got := <-ch:
		t.Fatalf("expected no notification on identical snapshot, got %v", got)
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestRuleScorer_VolumeDominatesWhenDeltasZero(t *testing.T) {
	s := NewRuleScorer(ScoringWeights{})
	out := s.Rank([]UniverseEntry{
		{Symbol: "A", QuoteVolume24h: 100},
		{Symbol: "B", QuoteVolume24h: 1000},
		{Symbol: "C", QuoteVolume24h: 500},
	})
	if out[0] != "B" {
		t.Fatalf("highest volume should rank first: %v", out)
	}
}

func TestRuleScorer_DeltaBeatsVolume(t *testing.T) {
	// A has highest volume but no momentum; B has lower volume but big 10m move.
	// Default weights make Delta10m equal to Volume, so a 1% move should
	// beat a 0.5% volume share difference.
	s := NewRuleScorer(ScoringWeights{})
	out := s.Rank([]UniverseEntry{
		{Symbol: "A", QuoteVolume24h: 1000, PriceChange10m: 0},
		{Symbol: "B", QuoteVolume24h: 500, PriceChange10m: 5}, // big move
	})
	if out[0] != "B" {
		t.Fatalf("momentum should win: %v", out)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
