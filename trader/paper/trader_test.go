package paper

import (
	"sync"
	"testing"
	"time"
)

// fixedMarkPrice gives the test deterministic prices; tests can flip it mid-run.
type fixedMarkPrice struct {
	mu    sync.Mutex
	price map[string]float64
}

func (f *fixedMarkPrice) get(symbol string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.price[symbol]; ok {
		return p, nil
	}
	return 0, errMissingPrice
}

func (f *fixedMarkPrice) set(symbol string, p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.price == nil {
		f.price = map[string]float64{}
	}
	f.price[symbol] = p
}

var errMissingPrice = mkErr("test mark price missing")

type errString string

func (e errString) Error() string { return string(e) }
func mkErr(s string) error        { return errString(s) }

func newPaperWith(t *testing.T, balance float64, prices map[string]float64) (*Trader, *fixedMarkPrice) {
	t.Helper()
	mp := &fixedMarkPrice{price: prices}
	tr, err := New(Config{
		InitialBalance: balance,
		FeeBps:         0, // disable fees so PnL math is exact
		MarkPriceFunc:  mp.get,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr, mp
}

func TestNew_RequiresPositiveBalance(t *testing.T) {
	cases := []struct {
		name    string
		balance float64
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"positive", 1000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{InitialBalance: tc.balance})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestOpenLong_ProfitableExit(t *testing.T) {
	tr, mp := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 50_000})

	if _, err := tr.OpenLong("BTCUSDT", 0.01, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}

	mp.set("BTCUSDT", 52_000)

	out, err := tr.CloseLong("BTCUSDT", 0)
	if err != nil {
		t.Fatalf("CloseLong: %v", err)
	}
	gotPnL, _ := out["realizedPnl"].(float64)
	wantPnL := (52_000.0 - 50_000.0) * 0.01 // = 20
	if gotPnL != wantPnL {
		t.Fatalf("realized PnL got=%.4f want=%.4f", gotPnL, wantPnL)
	}

	// balance should reflect realized PnL (no fees, fee bps=0)
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 10_020 {
		t.Fatalf("balance got=%.4f want=10020.00", got)
	}

	// position closed
	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("expected zero positions, got %d", len(pos))
	}
}

func TestOpenShort_LossExit(t *testing.T) {
	tr, mp := newPaperWith(t, 10_000, map[string]float64{"ETHUSDT": 3_000})

	if _, err := tr.OpenShort("ETHUSDT", 1, 3); err != nil {
		t.Fatalf("OpenShort: %v", err)
	}
	mp.set("ETHUSDT", 3_100)

	out, err := tr.CloseShort("ETHUSDT", 0)
	if err != nil {
		t.Fatalf("CloseShort: %v", err)
	}
	gotPnL, _ := out["realizedPnl"].(float64)
	want := (3_000.0 - 3_100.0) * 1.0 // -100
	if gotPnL != want {
		t.Fatalf("realized got=%.4f want=%.4f", gotPnL, want)
	}
}

func TestOpenLong_HedgeBlocked(t *testing.T) {
	tr, _ := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 50_000})
	if _, err := tr.OpenLong("BTCUSDT", 0.01, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	if _, err := tr.OpenShort("BTCUSDT", 0.01, 5); err == nil {
		t.Fatalf("expected hedge-mode rejection")
	}
}

func TestOpenLong_ScaleInWeightedAverage(t *testing.T) {
	tr, mp := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 100})

	if _, err := tr.OpenLong("BTCUSDT", 1, 1); err != nil { // 1 unit @ 100
		t.Fatalf("OpenLong#1: %v", err)
	}
	mp.set("BTCUSDT", 200)
	if _, err := tr.OpenLong("BTCUSDT", 1, 1); err != nil { // 1 unit @ 200
		t.Fatalf("OpenLong#2: %v", err)
	}

	pos, _ := tr.GetPositions()
	if len(pos) != 1 {
		t.Fatalf("want 1 position, got %d", len(pos))
	}
	avg, _ := pos[0]["entryPrice"].(float64)
	if avg != 150 {
		t.Fatalf("weighted avg entry got=%.4f want=150", avg)
	}
}

func TestUnrealizedPnL_TracksMark(t *testing.T) {
	tr, mp := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 100})
	if _, err := tr.OpenLong("BTCUSDT", 2, 1); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	mp.set("BTCUSDT", 110)

	pos, _ := tr.GetPositions()
	got, _ := pos[0]["unRealizedProfit"].(float64)
	if got != 20 {
		t.Fatalf("unrealized got=%.4f want=20", got)
	}
	bal, _ := tr.GetBalance()
	eq, _ := bal["total_equity"].(float64)
	if eq != 10_020 {
		t.Fatalf("equity got=%.4f want=10020", eq)
	}
}

func TestSetStopLoss_ListsAsOpenOrder(t *testing.T) {
	tr, _ := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 100})
	if _, err := tr.OpenLong("BTCUSDT", 1, 1); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	if err := tr.SetStopLoss("BTCUSDT", "LONG", 1, 90); err != nil {
		t.Fatalf("SetStopLoss: %v", err)
	}
	if err := tr.SetTakeProfit("BTCUSDT", "LONG", 1, 130); err != nil {
		t.Fatalf("SetTakeProfit: %v", err)
	}
	open, _ := tr.GetOpenOrders("BTCUSDT")
	if len(open) != 2 {
		t.Fatalf("want 2 open orders, got %d", len(open))
	}
}

func TestClose_DropsAssociatedOrders(t *testing.T) {
	tr, _ := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 100})
	_, _ = tr.OpenLong("BTCUSDT", 1, 1)
	_ = tr.SetStopLoss("BTCUSDT", "LONG", 1, 90)
	_ = tr.SetTakeProfit("BTCUSDT", "LONG", 1, 130)
	if _, err := tr.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatalf("CloseLong: %v", err)
	}
	open, _ := tr.GetOpenOrders("BTCUSDT")
	if len(open) != 0 {
		t.Fatalf("want 0 open orders after close, got %d", len(open))
	}
}

func TestGetClosedPnL_TimeFilter(t *testing.T) {
	tr, mp := newPaperWith(t, 10_000, map[string]float64{"BTCUSDT": 100})
	_, _ = tr.OpenLong("BTCUSDT", 1, 1)
	mp.set("BTCUSDT", 110)
	_, _ = tr.CloseLong("BTCUSDT", 0)

	all, _ := tr.GetClosedPnL(time.Time{}, 10)
	if len(all) != 1 {
		t.Fatalf("want 1 closed record, got %d", len(all))
	}

	future, _ := tr.GetClosedPnL(time.Now().Add(time.Hour), 10)
	if len(future) != 0 {
		t.Fatalf("filter-by-future-time: want 0 records, got %d", len(future))
	}
}

// ─── Contract-specific behaviour: margin lock + liquidation ───────────────

func TestOpen_RejectsWhenMarginExceedsAvailable(t *testing.T) {
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 50_000})
	// 0.05 BTC notional = 2500 USDT; at 1x leverage that's 2500 margin > 1000 wallet.
	if _, err := tr.OpenLong("BTCUSDT", 0.05, 1); err == nil {
		t.Fatalf("expected margin rejection when notional exceeds wallet")
	}
	// Same notional at 5x leverage → margin 500 → fits in 1000.
	if _, err := tr.OpenLong("BTCUSDT", 0.05, 5); err != nil {
		t.Fatalf("OpenLong with leverage that fits: %v", err)
	}
}

func TestOpen_RejectsAfterPartialMarginLocked(t *testing.T) {
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 50_000, "ETHUSDT": 1_000})
	// First open: 0.04 BTC * 50k / 5x = 400 margin; 600 still available.
	if _, err := tr.OpenLong("BTCUSDT", 0.04, 5); err != nil {
		t.Fatalf("OpenLong#1: %v", err)
	}
	// Second open: 1 ETH * 1k / 1x = 1000 margin; only 600 available → reject.
	if _, err := tr.OpenLong("ETHUSDT", 1, 1); err == nil {
		t.Fatalf("expected rejection when stacked margin exceeds wallet")
	}
	// Smaller second open inside remaining headroom should pass.
	if _, err := tr.OpenLong("ETHUSDT", 0.5, 1); err != nil {
		t.Fatalf("OpenLong#2 within headroom: %v", err)
	}
}

func TestLiquidation_LongTriggersOnDownCross(t *testing.T) {
	tr, mp := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	// 1 BTC at 100 with 5x lev → margin 20, liq ≈ 100 * (1 - 1/5 + 0.004) = 80.4
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	mp.set("BTCUSDT", 80) // pierces liq

	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("expected position liquidated, got %d remaining", len(pos))
	}
	// Wallet should drop by margin (20).
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 980 {
		t.Fatalf("balance after liquidation got=%.4f want=980.00", got)
	}
	// Liquidation should appear in closed records with CloseType=liquidation.
	closed, _ := tr.GetClosedPnL(time.Time{}, 10)
	if len(closed) != 1 || closed[0].CloseType != "liquidation" {
		t.Fatalf("expected one liquidation record, got %+v", closed)
	}
	if closed[0].RealizedPnL != -20 {
		t.Fatalf("liquidation realized got=%.4f want=-20", closed[0].RealizedPnL)
	}
}

func TestLiquidation_ShortTriggersOnUpCross(t *testing.T) {
	tr, mp := newPaperWith(t, 1_000, map[string]float64{"ETHUSDT": 100})
	// 1 ETH short at 100, 5x → margin 20, liq ≈ 100 * (1 + 1/5 - 0.004) = 119.6
	if _, err := tr.OpenShort("ETHUSDT", 1, 5); err != nil {
		t.Fatalf("OpenShort: %v", err)
	}
	mp.set("ETHUSDT", 120)
	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("expected liquidation, got %d positions", len(pos))
	}
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 980 {
		t.Fatalf("balance after liq got=%.4f want=980", got)
	}
}

func TestLiquidation_DoesNotTriggerInBuffer(t *testing.T) {
	tr, mp := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	// liq ≈ 80.4 at 5x. Drop to 81 — should NOT liquidate.
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	mp.set("BTCUSDT", 81)
	pos, _ := tr.GetPositions()
	if len(pos) != 1 {
		t.Fatalf("position should survive 81 vs liq 80.4, got liquidated")
	}
}

func TestSetMarginMode_StoresFlag(t *testing.T) {
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	if err := tr.SetMarginMode("BTCUSDT", true); err != nil {
		t.Fatalf("SetMarginMode: %v", err)
	}
	if !tr.isCrossMargin {
		t.Fatalf("isCrossMargin not stored")
	}
}

func TestFee_AppliedOnOpenAndClose(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 1_000}}
	tr, err := New(Config{InitialBalance: 10_000, FeeBps: 10, MarkPriceFunc: mp.get}) // 10 bps = 0.10%
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tr.OpenLong("BTCUSDT", 1, 1); err != nil { // notional 1000 → fee 1
		t.Fatalf("OpenLong: %v", err)
	}
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 9_999 {
		t.Fatalf("after open balance got=%.4f want=9999.00", got)
	}
	mp.set("BTCUSDT", 1_000) // flat
	if _, err := tr.CloseLong("BTCUSDT", 0); err != nil { // notional 1000 → fee 1, PnL 0
		t.Fatalf("CloseLong: %v", err)
	}
	bal, _ = tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 9_998 {
		t.Fatalf("after close balance got=%.4f want=9998.00", got)
	}
}
