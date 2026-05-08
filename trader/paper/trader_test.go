package paper

import (
	"sync"
	"testing"
	"time"

	"nofx/store"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

// ─── Funding rate accrual ─────────────────────────────────────────────────

// fixedFundingRate gives the test deterministic per-symbol funding rates.
type fixedFundingRate struct {
	mu   sync.Mutex
	rate map[string]float64
}

func (f *fixedFundingRate) get(symbol string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rate[symbol], nil
}

// stepClock returns whatever time was set on it; tests advance it explicitly.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}

func TestFunding_LongPaysWhenRatePositive(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	fr := &fixedFundingRate{rate: map[string]float64{"BTCUSDT": 0.01}} // 1% per cycle
	clk := &stepClock{t: time.Date(2026, 5, 7, 7, 30, 0, 0, time.UTC)}

	tr, err := New(Config{
		InitialBalance: 1_000, FeeBps: 0,
		MarkPriceFunc: mp.get, FundingRateFunc: fr.get, NowFunc: clk.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil { // notional 100, 5x → margin 20
		t.Fatalf("OpenLong: %v", err)
	}

	// No boundary crossed yet — same hour as open.
	tr.GetBalance()
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 1_000 {
		t.Fatalf("pre-funding balance moved unexpectedly: %.4f", got)
	}

	// Advance to 08:01 — crosses 08:00 boundary, one cycle of funding due.
	// long pays notional × rate = 100 × 0.01 = 1.0
	clk.set(time.Date(2026, 5, 7, 8, 1, 0, 0, time.UTC))
	bal, _ = tr.GetBalance()
	got, _ := bal["totalWalletBalance"].(float64)
	if got != 999 {
		t.Fatalf("after 1 funding cycle balance got=%.4f want=999.00", got)
	}

	// Idempotency: another GetBalance at the same clock should NOT re-charge.
	bal, _ = tr.GetBalance()
	if got2, _ := bal["totalWalletBalance"].(float64); got2 != 999 {
		t.Fatalf("funding double-charged: %.4f", got2)
	}
}

func TestFunding_ShortReceivesWhenRatePositive(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"ETHUSDT": 100}}
	fr := &fixedFundingRate{rate: map[string]float64{"ETHUSDT": 0.01}}
	clk := &stepClock{t: time.Date(2026, 5, 7, 7, 30, 0, 0, time.UTC)}

	tr, _ := New(Config{
		InitialBalance: 1_000, FeeBps: 0,
		MarkPriceFunc: mp.get, FundingRateFunc: fr.get, NowFunc: clk.now,
	})
	if _, err := tr.OpenShort("ETHUSDT", 1, 5); err != nil {
		t.Fatalf("OpenShort: %v", err)
	}
	clk.set(time.Date(2026, 5, 7, 8, 1, 0, 0, time.UTC))
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 1_001 {
		t.Fatalf("short funding credit: got=%.4f want=1001.00", got)
	}
}

func TestFunding_AccruesMultipleMissedCycles(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	fr := &fixedFundingRate{rate: map[string]float64{"BTCUSDT": 0.001}} // 0.1%
	clk := &stepClock{t: time.Date(2026, 5, 7, 7, 30, 0, 0, time.UTC)}

	tr, _ := New(Config{
		InitialBalance: 1_000, FeeBps: 0,
		MarkPriceFunc: mp.get, FundingRateFunc: fr.get, NowFunc: clk.now,
	})
	_, _ = tr.OpenLong("BTCUSDT", 1, 5)

	// Skip a full day → 3 funding cycles (08, 16, 00 next day).
	clk.set(time.Date(2026, 5, 8, 0, 30, 0, 0, time.UTC))
	bal, _ := tr.GetBalance()
	got, _ := bal["totalWalletBalance"].(float64)
	want := 1000 - 3*0.1 // 999.7
	if abs(got-want) > 1e-9 {
		t.Fatalf("3-cycle funding: got=%.6f want=%.6f", got, want)
	}
}

func TestFunding_NoOpWithZeroRate(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	fr := &fixedFundingRate{rate: map[string]float64{"BTCUSDT": 0}}
	clk := &stepClock{t: time.Date(2026, 5, 7, 7, 30, 0, 0, time.UTC)}

	tr, _ := New(Config{
		InitialBalance: 1_000, FeeBps: 0,
		MarkPriceFunc: mp.get, FundingRateFunc: fr.get, NowFunc: clk.now,
	})
	_, _ = tr.OpenLong("BTCUSDT", 1, 5)
	clk.set(time.Date(2026, 5, 8, 0, 30, 0, 0, time.UTC)) // 3 cycles passed
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 1_000 {
		t.Fatalf("zero-rate funding moved balance: %.4f", got)
	}
}

// ─── Stop-loss / take-profit auto-trigger ─────────────────────────────────

func TestStopLoss_LongFiresOnDownCross(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	tr.getMarkPrice = mp.get // re-bind so we can move price after construction

	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	if err := tr.SetStopLoss("BTCUSDT", "LONG", 1, 95); err != nil {
		t.Fatalf("SetStopLoss: %v", err)
	}

	mp.set("BTCUSDT", 94) // below SL trigger
	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("SL should have fired; %d positions remain", len(pos))
	}
	closed, _ := tr.GetClosedPnL(time.Time{}, 10)
	if len(closed) != 1 || closed[0].CloseType != "stop_loss" {
		t.Fatalf("expected one stop_loss record, got %+v", closed)
	}
	if closed[0].ExitPrice != 95 {
		t.Fatalf("SL fill price got=%.4f want=95 (trigger price, V1)", closed[0].ExitPrice)
	}
	// realized = (95-100)*1 = -5, fee 0 (no fee in this test)
	if closed[0].RealizedPnL != -5 {
		t.Fatalf("SL realized got=%.4f want=-5", closed[0].RealizedPnL)
	}
}

func TestTakeProfit_LongFiresOnUpCross(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	tr.getMarkPrice = mp.get

	_, _ = tr.OpenLong("BTCUSDT", 1, 5)
	_ = tr.SetTakeProfit("BTCUSDT", "LONG", 1, 110)

	mp.set("BTCUSDT", 111)
	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("TP should have fired")
	}
	closed, _ := tr.GetClosedPnL(time.Time{}, 10)
	if closed[0].CloseType != "take_profit" {
		t.Fatalf("expected take_profit, got %s", closed[0].CloseType)
	}
	if closed[0].RealizedPnL != 10 {
		t.Fatalf("TP realized got=%.4f want=10", closed[0].RealizedPnL)
	}
}

func TestStopLoss_ShortFiresOnUpCross(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"ETHUSDT": 100}}
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"ETHUSDT": 100})
	tr.getMarkPrice = mp.get

	_, _ = tr.OpenShort("ETHUSDT", 1, 5)
	_ = tr.SetStopLoss("ETHUSDT", "SHORT", 1, 105)

	mp.set("ETHUSDT", 106)
	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("short SL should have fired")
	}
}

func TestTriggers_OppositeOrderCancelledOnFill(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	tr.getMarkPrice = mp.get

	_, _ = tr.OpenLong("BTCUSDT", 1, 5)
	_ = tr.SetStopLoss("BTCUSDT", "LONG", 1, 95)
	_ = tr.SetTakeProfit("BTCUSDT", "LONG", 1, 110)

	mp.set("BTCUSDT", 111) // TP fires
	_, _ = tr.GetPositions()
	open, _ := tr.GetOpenOrders("BTCUSDT")
	if len(open) != 0 {
		t.Fatalf("SL should have been cancelled when TP fired; remaining: %d", len(open))
	}
}

func TestTriggers_DoNotFireWhenMarkInsideRange(t *testing.T) {
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	tr.getMarkPrice = mp.get

	_, _ = tr.OpenLong("BTCUSDT", 1, 5)
	_ = tr.SetStopLoss("BTCUSDT", "LONG", 1, 95)
	_ = tr.SetTakeProfit("BTCUSDT", "LONG", 1, 110)

	mp.set("BTCUSDT", 102)
	pos, _ := tr.GetPositions()
	if len(pos) != 1 {
		t.Fatalf("position should still be open at 102, got %d", len(pos))
	}
	open, _ := tr.GetOpenOrders("BTCUSDT")
	if len(open) != 2 {
		t.Fatalf("orders should still be open, got %d", len(open))
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ─── Cross margin ─────────────────────────────────────────────────────────

func newCrossPaper(t *testing.T, balance float64, prices map[string]float64) (*Trader, *fixedMarkPrice) {
	t.Helper()
	tr, mp := newPaperWith(t, balance, prices)
	if err := tr.SetMarginMode("ANY", true); err != nil {
		t.Fatalf("SetMarginMode(cross): %v", err)
	}
	return tr, mp
}

func TestCrossMargin_HealthyAccountSurvivesPriceMoveBelowIsolatedLiq(t *testing.T) {
	tr, mp := newCrossPaper(t, 1_000, map[string]float64{"BTCUSDT": 100})

	// 1 BTC long @ 100, 5x → notional 100, margin 20.
	// Isolated liq ≈ 100 × (1 - 0.2 + 0.004) = 80.4.
	// Cross liq ≈ (100 - 1000) / (1 × 0.996) = -903 (negative → 0; effectively no liq risk).
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}

	mp.set("BTCUSDT", 75) // well below isolated liq, but cross has the whole wallet as buffer
	pos, _ := tr.GetPositions()
	if len(pos) != 1 {
		t.Fatalf("cross-margin should keep position alive at mark 75; got %d positions", len(pos))
	}
}

func TestCrossMargin_AccountLiquidationClosesAllPositions(t *testing.T) {
	tr, mp := newCrossPaper(t, 100, map[string]float64{"ETHUSDT": 100, "BTCUSDT": 100})

	// One short + one long. Mark moving up crushes the short unboundedly while
	// also boosting the long; with the right qty mix the short loss outpaces
	// the long gain and wipes the wallet — exactly the failure mode cross
	// margin is supposed to catch.
	//
	// 1.0 ETH short @ 100 × 1x   notional 100, margin 100
	// margin_lock allows this because total locked = 100 = wallet.
	if _, err := tr.OpenShort("ETHUSDT", 1, 1); err != nil {
		t.Fatalf("OpenShort: %v", err)
	}

	// ETH 10x → short unrealized = (100 - 1000) × 1 = -900.
	// margin_balance = 100 + (-900) = -800.
	// maintenance = 1000 × 1 × 0.004 = 4. -800 < 4 → liquidate.
	mp.set("ETHUSDT", 1_000)

	pos, _ := tr.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("cross account should be liquidated, %d positions remain", len(pos))
	}
	bal, _ := tr.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 0 {
		t.Fatalf("cross liquidation should clamp balance to 0, got %.4f", got)
	}
	closed, _ := tr.GetClosedPnL(time.Time{}, 10)
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed record, got %d", len(closed))
	}
	if closed[0].CloseType != "liquidation" {
		t.Fatalf("close type should be liquidation, got %s", closed[0].CloseType)
	}
}

func TestCrossMargin_LiqPriceWidensWithProfitableHedge(t *testing.T) {
	tr, mp := newCrossPaper(t, 200, map[string]float64{"BTCUSDT": 100, "ETHUSDT": 100})

	// 1 BTC long @ 100, 5x. Cross liq with no hedge: K = 200 → liq = (100 - 200)/0.996 ≈ -100 → clamped 0.
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong BTC: %v", err)
	}
	posBeforeHedge, _ := tr.GetPositions()
	liqBefore, _ := posBeforeHedge[0]["liquidationPrice"].(float64)

	// Add a profitable short hedge that gains as BTC moves with the down case
	// (say ETH short — ETH goes up = ETH short loses, so for a "supportive" hedge
	// we'd want ETH long that profits when other moves). Use a flat ETH long
	// whose unrealized rises as we crank ETH price up.
	if _, err := tr.OpenLong("ETHUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong ETH: %v", err)
	}
	mp.set("ETHUSDT", 200) // ETH +100% → +100 USDT unrealized for the ETH long

	// Now the BTC cross liq should be lower (further from current price) because
	// the unrealized ETH gain widens K.
	posAfter, _ := tr.GetPositions()
	var liqBTC float64
	for _, p := range posAfter {
		if p["symbol"] == "BTCUSDT" {
			liqBTC, _ = p["liquidationPrice"].(float64)
		}
	}
	if !(liqBTC <= liqBefore) {
		t.Fatalf("BTC cross liq should not increase when ETH hedge profits; before=%.4f after=%.4f", liqBefore, liqBTC)
	}
}

func TestSetMarginMode_RejectsWhileOpenPositions(t *testing.T) {
	tr, _ := newPaperWith(t, 1_000, map[string]float64{"BTCUSDT": 100})
	if _, err := tr.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	if err := tr.SetMarginMode("BTCUSDT", true); err == nil {
		t.Fatalf("expected SetMarginMode to reject mode change with open position")
	}
	// Idempotent same-mode should still succeed.
	if err := tr.SetMarginMode("BTCUSDT", false); err != nil {
		t.Fatalf("idempotent same-mode set should succeed: %v", err)
	}
}

func TestCrossMargin_IsolatedAndCrossUseDifferentLiqDisplay(t *testing.T) {
	// Same position; only margin mode differs → liq prices differ.
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}
	tr1, err := New(Config{InitialBalance: 1_000, FeeBps: 0, MarkPriceFunc: mp.get})
	if err != nil {
		t.Fatalf("New isolated: %v", err)
	}
	if _, err := tr1.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	pos1, _ := tr1.GetPositions()
	liqIso, _ := pos1[0]["liquidationPrice"].(float64)

	tr2, _ := New(Config{InitialBalance: 1_000, FeeBps: 0, MarkPriceFunc: mp.get})
	_ = tr2.SetMarginMode("BTCUSDT", true)
	if _, err := tr2.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong cross: %v", err)
	}
	pos2, _ := tr2.GetPositions()
	liqCross, _ := pos2[0]["liquidationPrice"].(float64)

	// Cross with a 1000 USDT wallet on a 100 USDT notional position has effectively
	// no near-term liq risk, so its liq price should be much lower than isolated's 80.4.
	if !(liqCross < liqIso) {
		t.Fatalf("cross liq should be lower than isolated for the same position; iso=%.4f cross=%.4f", liqIso, liqCross)
	}
}

// ─── Persistence ───────────────────────────────────────────────────────────

// newPaperStore constructs an isolated in-memory SQLite-backed store for tests.
func newPaperStore(t *testing.T) *store.Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&store.PaperState{}); err != nil {
		t.Fatalf("migrate paper_states: %v", err)
	}
	st, err := store.NewFromGorm(gdb)
	if err != nil {
		t.Fatalf("NewFromGorm: %v", err)
	}
	return st
}

func TestPersistence_RoundTripsAcrossReinit(t *testing.T) {
	st := newPaperStore(t)
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}

	tr1, err := New(Config{
		InitialBalance: 10_000, FeeBps: 0,
		MarkPriceFunc: mp.get,
		Store:         st, TraderID: "test-trader-1",
	})
	if err != nil {
		t.Fatalf("New#1: %v", err)
	}

	if _, err := tr1.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	if err := tr1.SetStopLoss("BTCUSDT", "LONG", 1, 90); err != nil {
		t.Fatalf("SetStopLoss: %v", err)
	}
	mp.set("BTCUSDT", 110)
	if _, err := tr1.CloseLong("BTCUSDT", 0.5); err != nil { // partial close
		t.Fatalf("CloseLong: %v", err)
	}

	bal1Map, _ := tr1.GetBalance()
	bal1, _ := bal1Map["totalWalletBalance"].(float64)
	pos1, _ := tr1.GetPositions()
	closed1, _ := tr1.GetClosedPnL(time.Time{}, 100)

	// Drop tr1 entirely and rebuild from the same exchange id.
	tr2, err := New(Config{
		InitialBalance: 999_999, // ignored when state hydrates
		FeeBps:         0,
		MarkPriceFunc:  mp.get,
		Store:          st, TraderID: "test-trader-1",
	})
	if err != nil {
		t.Fatalf("New#2 (hydrate): %v", err)
	}

	bal2Map, _ := tr2.GetBalance()
	bal2, _ := bal2Map["totalWalletBalance"].(float64)
	if bal1 != bal2 {
		t.Fatalf("balance not restored: pre=%.4f post=%.4f", bal1, bal2)
	}

	pos2, _ := tr2.GetPositions()
	if len(pos1) != len(pos2) {
		t.Fatalf("positions count mismatch: pre=%d post=%d", len(pos1), len(pos2))
	}
	if len(pos2) == 1 {
		entry, _ := pos2[0]["entryPrice"].(float64)
		if entry != 100 {
			t.Fatalf("entry price not restored: %v", entry)
		}
	}

	closed2, _ := tr2.GetClosedPnL(time.Time{}, 100)
	if len(closed1) != len(closed2) {
		t.Fatalf("closed records count mismatch: pre=%d post=%d", len(closed1), len(closed2))
	}
}

func TestPersistence_FreshExchangeRequiresInitialBalance(t *testing.T) {
	st := newPaperStore(t)
	if _, err := New(Config{
		InitialBalance: 0, // missing
		Store:          st, TraderID: "fresh-no-balance",
	}); err == nil {
		t.Fatalf("expected error when no persisted state and no InitialBalance")
	}
}

func TestPersistence_StoreAndTraderIDMustAgree(t *testing.T) {
	st := newPaperStore(t)
	if _, err := New(Config{InitialBalance: 1000, Store: st}); err == nil {
		t.Fatalf("expected error when Store set without TraderID")
	}
	if _, err := New(Config{InitialBalance: 1000, TraderID: "x"}); err == nil {
		t.Fatalf("expected error when TraderID set without Store")
	}
}

func TestPersistence_LiquidationSurvivesRestart(t *testing.T) {
	st := newPaperStore(t)
	mp := &fixedMarkPrice{price: map[string]float64{"BTCUSDT": 100}}

	tr1, _ := New(Config{
		InitialBalance: 1_000, FeeBps: 0,
		MarkPriceFunc: mp.get,
		Store:         st, TraderID: "liq-test",
	})
	if _, err := tr1.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	mp.set("BTCUSDT", 80) // triggers liquidation when next read happens
	_, _ = tr1.GetPositions()  // forces settlement + persist

	// Reincarnate from store. Even though mark is now 80 (still below liq),
	// the position has already been liquidated and recorded as a closed entry.
	tr2, _ := New(Config{
		InitialBalance: 999_999, // ignored — hydrated
		FeeBps:         0,
		MarkPriceFunc:  mp.get,
		Store:          st, TraderID: "liq-test",
	})

	bal, _ := tr2.GetBalance()
	if got, _ := bal["totalWalletBalance"].(float64); got != 980 {
		t.Fatalf("balance after liquidation+restart got=%.4f want=980", got)
	}
	pos, _ := tr2.GetPositions()
	if len(pos) != 0 {
		t.Fatalf("expected zero positions after liquidation+restart, got %d", len(pos))
	}
	closed, _ := tr2.GetClosedPnL(time.Time{}, 10)
	if len(closed) != 1 || closed[0].CloseType != "liquidation" {
		t.Fatalf("liquidation record not restored: %+v", closed)
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
