// Package paper provides a virtual exchange that implements the trader.Trader
// interface using in-memory positions and live mark prices fetched from the
// market data layer. It lets the rest of NOFX (auto loop, AI decisions, risk
// layer, store, dashboard) run end-to-end without ever touching a real venue.
//
// Scope (V1):
//   - Per-symbol single position (no hedge mode). One long OR one short per symbol.
//   - Market-only fills at the latest mark price returned by market.Get.
//   - Stop-loss / take-profit are stored as conditional orders but not auto-triggered;
//     the framework risk layer (auto_trader_risk.go) is responsible for calling
//     CloseLong / CloseShort when its own monitor fires.
//   - State is in-memory; restarting the process resets the paper account.
//   - Optional taker fee in basis points (default 5 bps = 0.05%).
//
// What it does not do (V1):
//   - No partial fills, no slippage modelling beyond a flat fee.
//   - No funding-rate accrual.
//   - No cross-margin auto-deleveraging or liquidation engine.
//   - No persistence; positions and balance reset on restart.
package paper

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

// MarkPriceFunc returns the current mark price for a symbol. It is parameterised
// so tests can inject deterministic prices instead of hitting the real market.
type MarkPriceFunc func(symbol string) (float64, error)

// FundingRateFunc returns the current 8h funding rate for a symbol. Sign
// follows industry convention: positive = longs pay shorts.
type FundingRateFunc func(symbol string) (float64, error)

// NowFunc returns the current time. Tests inject deterministic clocks; production
// uses time.Now.
type NowFunc func() time.Time

// DefaultMarkPriceFunc resolves the price via the production market data layer.
func DefaultMarkPriceFunc(symbol string) (float64, error) {
	d, err := market.Get(symbol)
	if err != nil {
		return 0, fmt.Errorf("paper: market.Get(%s): %w", symbol, err)
	}
	if d == nil || d.CurrentPrice <= 0 {
		return 0, fmt.Errorf("paper: market.Get(%s) returned no price", symbol)
	}
	return d.CurrentPrice, nil
}

// DefaultFundingRateFunc reads the latest funding rate from the production
// market data layer. Returns 0 (no funding) if the venue doesn't publish one
// for that symbol — that's the most conservative default for paper.
func DefaultFundingRateFunc(symbol string) (float64, error) {
	d, err := market.Get(symbol)
	if err != nil {
		return 0, nil // soft fail: skip funding this tick rather than break the loop
	}
	if d == nil {
		return 0, nil
	}
	return d.FundingRate, nil
}

// FundingIntervalHours is the cadence at which funding is settled. 8h matches
// Binance / OKX / Bybit USDT-M perp convention; some venues use 1h or 4h.
// Constant here for simplicity — promote to Config when V2 adds per-symbol overrides.
const FundingIntervalHours = 8

// Position represents an open virtual position.
type Position struct {
	Symbol     string
	Side       string // "long" or "short"
	Quantity   float64
	EntryPrice float64
	Leverage   int
	OpenedAt   time.Time
}

// orderKind enumerates the conditional order types we track.
type orderKind string

const (
	orderStopLoss   orderKind = "STOP_MARKET"
	orderTakeProfit orderKind = "TAKE_PROFIT_MARKET"
	orderLimit      orderKind = "LIMIT"
)

type pendingOrder struct {
	id           string
	symbol       string
	side         string // "BUY" / "SELL"
	positionSide string // "LONG" / "SHORT"
	kind         orderKind
	price        float64 // limit price
	stopPrice    float64 // trigger price for stop / take-profit
	quantity     float64
	createdAt    time.Time
}

// MaintenanceMarginRate is the maintenance-margin haircut applied when computing
// liquidation prices. 0.4% mirrors Binance Futures USDT-M baseline; real venues
// vary by symbol and tier — V1 uses a single rate for simplicity.
const MaintenanceMarginRate = 0.004

// closedRecordCap caps how many closed PnL records are retained in memory and
// persisted. Older records are dropped — long-term history belongs in the
// framework's trader_orders/decision_records tables.
const closedRecordCap = 500

// Trader is the in-memory virtual exchange.
type Trader struct {
	mu sync.RWMutex

	// Settings — set once at construction; read-only afterwards.
	feeBps          float64         // taker fee in basis points; e.g. 5 = 0.05%
	getMarkPrice    MarkPriceFunc   // injectable for tests
	getFundingRate  FundingRateFunc // injectable for tests
	now             NowFunc         // injectable clock; production uses time.Now
	leverageBySym   map[string]int

	// Persistence — optional. Both must be set together or both nil/empty.
	store      *store.Store
	exchangeID string

	// Mutable state.
	balance         float64                  // wallet balance (USDT). Only fees, realized PnL, liquidation losses, and funding payments move this.
	positions       map[string]*Position     // by symbol
	orders          map[string]*pendingOrder // pending stop-loss / take-profit / limit orders
	closed          []types.ClosedPnLRecord
	orderSeq        uint64
	isCrossMargin   bool      // false = isolated (V1 default; liquidation math assumes isolated)
	lastFundingTime time.Time // most recent funding boundary that has been applied
}

// Config is the paper trader configuration.
type Config struct {
	InitialBalance  float64         // starting USDT balance; required (>0) when no persisted state exists
	FeeBps          float64         // optional taker fee bps (default 5)
	MarkPriceFunc   MarkPriceFunc   // optional override (defaults to live market data)
	FundingRateFunc FundingRateFunc // optional override (defaults to live market data; tests inject deterministic rates)
	NowFunc         NowFunc         // optional override (defaults to time.Now; tests inject deterministic clocks)

	// Persistence (both required together, both optional). When provided, the
	// trader hydrates from store on construction and writes the full state
	// back after every mutation. When omitted, the trader is purely in-memory
	// and resets on restart.
	Store      *store.Store
	ExchangeID string
}

// New constructs a paper Trader.
//
// FeeBps is honoured exactly: zero means zero fees. Negative values are rejected.
// Callers wiring the paper trader into auto_trader.go should pass an explicit
// fee (5 bps is a reasonable default for a generic CEX).
//
// When cfg.Store and cfg.ExchangeID are both set, the trader hydrates state
// from the persisted row. If no row exists, it initializes with cfg.InitialBalance
// and immediately writes a fresh row. Subsequent mutations persist after each
// state change. When persistence is not configured, the trader is in-memory only
// and resets on process restart.
func New(cfg Config) (*Trader, error) {
	if cfg.FeeBps < 0 {
		return nil, fmt.Errorf("paper: FeeBps must be >= 0")
	}
	mp := cfg.MarkPriceFunc
	if mp == nil {
		mp = DefaultMarkPriceFunc
	}
	fr := cfg.FundingRateFunc
	if fr == nil {
		fr = DefaultFundingRateFunc
	}
	nw := cfg.NowFunc
	if nw == nil {
		nw = func() time.Time { return time.Now().UTC() }
	}

	// Persistence is opt-in but all-or-nothing — both fields must agree.
	persistEnabled := cfg.Store != nil && cfg.ExchangeID != ""
	if (cfg.Store != nil) != (cfg.ExchangeID != "") {
		return nil, fmt.Errorf("paper: Store and ExchangeID must be set together or both omitted")
	}

	t := &Trader{
		feeBps:         cfg.FeeBps,
		getMarkPrice:   mp,
		getFundingRate: fr,
		now:            nw,
		leverageBySym:  make(map[string]int),
		store:          cfg.Store,
		exchangeID:     cfg.ExchangeID,
		positions:      make(map[string]*Position),
		orders:         make(map[string]*pendingOrder),
	}

	if persistEnabled {
		hydrated, err := t.tryHydrate()
		if err != nil {
			// Hydration shouldn't be fatal — log loudly and start fresh so a
			// corrupted blob doesn't lock the whole exchange out.
			logger.Warnf("📄 [paper] hydrate failed for %s, starting fresh: %v", cfg.ExchangeID, err)
		}
		if !hydrated {
			if cfg.InitialBalance <= 0 {
				return nil, fmt.Errorf("paper: InitialBalance must be > 0 (no persisted state for exchange %s)", cfg.ExchangeID)
			}
			t.balance = cfg.InitialBalance
			if err := t.persistLocked(); err != nil {
				logger.Warnf("📄 [paper] initial persist failed: %v", err)
			}
			logger.Infof("📄 [paper] initialized fresh: exchange=%s balance=%.2f USDT, fee=%.2fbps",
				cfg.ExchangeID, cfg.InitialBalance, cfg.FeeBps)
		} else {
			logger.Infof("📄 [paper] hydrated from store: exchange=%s balance=%.2f, positions=%d, fee=%.2fbps",
				cfg.ExchangeID, t.balance, len(t.positions), cfg.FeeBps)
		}
		return t, nil
	}

	// No persistence — pure in-memory mode, balance is required.
	if cfg.InitialBalance <= 0 {
		return nil, fmt.Errorf("paper: InitialBalance must be > 0")
	}
	t.balance = cfg.InitialBalance
	logger.Infof("📄 [paper] initialized in-memory: balance=%.2f USDT, fee=%.2fbps",
		cfg.InitialBalance, cfg.FeeBps)
	return t, nil
}

// snapshot is the JSON shape used by persistence. It deliberately mirrors the
// in-memory state 1:1 so unmarshal can reconstitute without translation.
type snapshot struct {
	Positions map[string]*Position    `json:"positions"`
	Orders    map[string]*pendingOrder `json:"orders"`
	Closed    []types.ClosedPnLRecord  `json:"closed"`
}

// pendingOrder is unexported; expose its fields for JSON round-trip via custom
// marshal so persistence doesn't require renaming or moving the type.
type pendingOrderJSON struct {
	ID           string    `json:"id"`
	Symbol       string    `json:"symbol"`
	Side         string    `json:"side"`
	PositionSide string    `json:"position_side"`
	Kind         orderKind `json:"kind"`
	Price        float64   `json:"price"`
	StopPrice    float64   `json:"stop_price"`
	Quantity     float64   `json:"quantity"`
	CreatedAt    time.Time `json:"created_at"`
}

func (o *pendingOrder) MarshalJSON() ([]byte, error) {
	return json.Marshal(pendingOrderJSON{
		ID: o.id, Symbol: o.symbol, Side: o.side, PositionSide: o.positionSide,
		Kind: o.kind, Price: o.price, StopPrice: o.stopPrice, Quantity: o.quantity, CreatedAt: o.createdAt,
	})
}

func (o *pendingOrder) UnmarshalJSON(b []byte) error {
	var aux pendingOrderJSON
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	o.id, o.symbol, o.side, o.positionSide = aux.ID, aux.Symbol, aux.Side, aux.PositionSide
	o.kind, o.price, o.stopPrice, o.quantity, o.createdAt = aux.Kind, aux.Price, aux.StopPrice, aux.Quantity, aux.CreatedAt
	return nil
}

// tryHydrate loads state from the store. Returns true if a row was found and
// successfully applied; false if no row exists (caller should init fresh).
func (t *Trader) tryHydrate() (bool, error) {
	row, err := t.store.PaperState().Get(t.exchangeID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, nil
	}

	var snap snapshot
	// Empty blobs are fine — treat them as empty maps/slices.
	if row.PositionsJSON != "" {
		if err := json.Unmarshal([]byte(row.PositionsJSON), &snap.Positions); err != nil {
			return false, fmt.Errorf("decode positions: %w", err)
		}
	}
	if row.OrdersJSON != "" {
		if err := json.Unmarshal([]byte(row.OrdersJSON), &snap.Orders); err != nil {
			return false, fmt.Errorf("decode orders: %w", err)
		}
	}
	if row.ClosedJSON != "" {
		if err := json.Unmarshal([]byte(row.ClosedJSON), &snap.Closed); err != nil {
			return false, fmt.Errorf("decode closed: %w", err)
		}
	}

	if snap.Positions != nil {
		t.positions = snap.Positions
	}
	if snap.Orders != nil {
		t.orders = snap.Orders
	}
	t.closed = snap.Closed
	t.balance = row.Balance
	t.isCrossMargin = row.IsCrossMargin
	t.lastFundingTime = row.LastFundingTime
	atomic.StoreUint64(&t.orderSeq, row.OrderSeq)
	return true, nil
}

// persistLocked writes the current state to the store. Caller MUST hold t.mu
// (write). Failures are returned but callers typically log and continue —
// losing one snapshot is recoverable next tick.
func (t *Trader) persistLocked() error {
	if t.store == nil || t.exchangeID == "" {
		return nil // persistence disabled
	}

	// Cap closed history before serialization so the blob doesn't grow forever.
	if len(t.closed) > closedRecordCap {
		t.closed = t.closed[len(t.closed)-closedRecordCap:]
	}

	posJSON, err := json.Marshal(t.positions)
	if err != nil {
		return fmt.Errorf("marshal positions: %w", err)
	}
	ordJSON, err := json.Marshal(t.orders)
	if err != nil {
		return fmt.Errorf("marshal orders: %w", err)
	}
	clsJSON, err := json.Marshal(t.closed)
	if err != nil {
		return fmt.Errorf("marshal closed: %w", err)
	}

	row := &store.PaperState{
		ExchangeID:      t.exchangeID,
		Balance:         t.balance,
		IsCrossMargin:   t.isCrossMargin,
		OrderSeq:        atomic.LoadUint64(&t.orderSeq),
		LastFundingTime: t.lastFundingTime,
		PositionsJSON:   string(posJSON),
		OrdersJSON:      string(ordJSON),
		ClosedJSON:      string(clsJSON),
	}
	return t.store.PaperState().Save(row)
}

// persistOrLog is a convenience that swallows persist errors via the logger.
// Used inside mutation paths where we don't want a transient DB error to break
// the in-memory operation; the caller has already mutated state.
func (t *Trader) persistOrLog(op string) {
	if err := t.persistLocked(); err != nil {
		logger.Warnf("📄 [paper] persist after %s failed: %v", op, err)
	}
}

// nextOrderID returns a monotonically increasing unique paper order id.
func (t *Trader) nextOrderID(prefix string) string {
	n := atomic.AddUint64(&t.orderSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

// ---------------------------------------------------------------------------
// types.Trader implementation
// ---------------------------------------------------------------------------

// GetBalance returns the wallet balance and aggregate equity in the same shape
// callers expect (matches the multi-key fallback in auto_trader.go). Liquidation
// settlement runs first so a freshly-blown position is reflected immediately.
func (t *Trader) GetBalance() (map[string]interface{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tickLocked()

	unrealized := 0.0
	margin := 0.0
	for _, p := range t.positions {
		px, err := t.getMarkPrice(p.Symbol)
		if err != nil {
			// Don't fail the entire balance call on a single missing tick;
			// just skip that position's unrealized component.
			continue
		}
		unrealized += unrealizedPnL(p, px)
		margin += positionMargin(p)
	}
	equity := t.balance + unrealized
	out := map[string]interface{}{
		"totalWalletBalance": t.balance,
		"total_equity":       equity,
		"availableBalance":   t.balance - margin,
		"unrealizedPnL":      unrealized,
		"marginUsed":         margin,
	}
	return out, nil
}

// GetPositions returns all open positions in the Binance-shaped map slice.
// Liquidation settlement runs first so blown positions disappear from the result.
func (t *Trader) GetPositions() ([]map[string]interface{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tickLocked()

	out := make([]map[string]interface{}, 0, len(t.positions))
	for _, p := range t.positions {
		mark, err := t.getMarkPrice(p.Symbol)
		if err != nil {
			mark = p.EntryPrice
		}
		amt := p.Quantity
		if p.Side == "short" {
			amt = -p.Quantity
		}
		entry := p.EntryPrice
		out = append(out, map[string]interface{}{
			"symbol":           p.Symbol,
			"positionAmt":      amt,
			"entryPrice":       entry,
			"markPrice":        mark,
			"unRealizedProfit": unrealizedPnL(p, mark),
			"leverage":         float64(p.Leverage),
			"liquidationPrice": liquidationPrice(p),
			"side":             p.Side,
		})
	}
	return out, nil
}

// OpenLong fills a market buy at the live mark price.
func (t *Trader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openPosition(symbol, "long", quantity, leverage)
}

// OpenShort fills a market sell at the live mark price.
func (t *Trader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openPosition(symbol, "short", quantity, leverage)
}

func (t *Trader) openPosition(symbol, side string, quantity float64, leverage int) (map[string]interface{}, error) {
	if quantity <= 0 {
		return nil, fmt.Errorf("paper: open quantity must be > 0")
	}
	if leverage <= 0 {
		leverage = 1
	}
	price, err := t.getMarkPrice(symbol)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Run a tick before the margin check so a freshly-blown position frees its
	// margin and any newly-triggered SL/TP free their orders.
	t.tickLocked()

	notional := quantity * price
	fee := notional * (t.feeBps / 10000.0)
	newMargin := notional / float64(leverage)

	// [CONTRACT] Margin lock — reject if the wallet can't cover the new
	// initial margin plus fees on top of already-locked margin.
	available := t.balance - t.totalMarginLockedLocked()
	if newMargin+fee > available {
		return nil, fmt.Errorf(
			"paper: insufficient margin for %s %s: need %.2f (margin %.2f + fee %.2f), available %.2f",
			side, symbol, newMargin+fee, newMargin, fee, available)
	}

	if existing, ok := t.positions[symbol]; ok {
		if existing.Side != side {
			return nil, fmt.Errorf("paper: cannot open %s on %s while a %s position is open (V1 = no hedge mode)", side, symbol, existing.Side)
		}
		// Same-side scale-in: weighted-average entry, sum quantity.
		newQty := existing.Quantity + quantity
		existing.EntryPrice = ((existing.EntryPrice * existing.Quantity) + (price * quantity)) / newQty
		existing.Quantity = newQty
		existing.Leverage = leverage
	} else {
		t.positions[symbol] = &Position{
			Symbol:     symbol,
			Side:       side,
			Quantity:   quantity,
			EntryPrice: price,
			Leverage:   leverage,
			OpenedAt:   time.Now().UTC(),
		}
	}

	// Charge taker fee against balance.
	t.balance -= fee

	id := t.nextOrderID("OPEN")
	logger.Infof("📄 [paper] OPEN %s %s qty=%.6f @ %.4f lev=%dx fee=%.4f balance=%.2f",
		strings.ToUpper(side), symbol, quantity, price, leverage, fee, t.balance)

	t.persistOrLog("open")

	return map[string]interface{}{
		"orderId":     id,
		"symbol":      symbol,
		"status":      "FILLED",
		"avgPrice":    price,
		"executedQty": quantity,
		"commission":  fee,
	}, nil
}

// CloseLong closes a long position partially or fully (quantity == 0 closes all).
func (t *Trader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closePosition(symbol, "long", quantity)
}

// CloseShort closes a short position partially or fully.
func (t *Trader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closePosition(symbol, "short", quantity)
}

func (t *Trader) closePosition(symbol, expectedSide string, quantity float64) (map[string]interface{}, error) {
	price, err := t.getMarkPrice(symbol)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	pos, ok := t.positions[symbol]
	if !ok {
		return nil, fmt.Errorf("paper: no open position for %s", symbol)
	}
	if pos.Side != expectedSide {
		return nil, fmt.Errorf("paper: position on %s is %s, not %s", symbol, pos.Side, expectedSide)
	}
	closeQty := quantity
	if closeQty <= 0 || closeQty > pos.Quantity {
		closeQty = pos.Quantity
	}

	realized := realizedPnL(pos, price, closeQty)
	notional := closeQty * price
	fee := notional * (t.feeBps / 10000.0)
	t.balance += realized - fee

	t.dropAssociatedOrders(symbol)

	rec := types.ClosedPnLRecord{
		Symbol:      symbol,
		Side:        pos.Side,
		EntryPrice:  pos.EntryPrice,
		ExitPrice:   price,
		Quantity:    closeQty,
		RealizedPnL: realized - fee,
		Fee:         fee,
		Leverage:    pos.Leverage,
		EntryTime:   pos.OpenedAt,
		ExitTime:    time.Now().UTC(),
		OrderID:     t.nextOrderID("CLOSE"),
		CloseType:   "manual",
		ExchangeID:  "paper",
	}
	t.closed = append(t.closed, rec)

	if closeQty >= pos.Quantity {
		delete(t.positions, symbol)
	} else {
		pos.Quantity -= closeQty
	}

	logger.Infof("📄 [paper] CLOSE %s %s qty=%.6f @ %.4f realized=%.4f fee=%.4f balance=%.2f",
		strings.ToUpper(pos.Side), symbol, closeQty, price, realized, fee, t.balance)

	t.persistOrLog("close")

	return map[string]interface{}{
		"orderId":     rec.OrderID,
		"symbol":      symbol,
		"status":      "FILLED",
		"avgPrice":    price,
		"executedQty": closeQty,
		"realizedPnl": realized - fee,
		"commission":  fee,
	}, nil
}

// SetLeverage records the leverage for a symbol; paper has no real-side state.
// Symbol-level leverage isn't persisted (it's overwritten by every Open call's
// leverage param anyway).
func (t *Trader) SetLeverage(symbol string, leverage int) error {
	if leverage <= 0 {
		return fmt.Errorf("paper: leverage must be > 0")
	}
	t.mu.Lock()
	t.leverageBySym[symbol] = leverage
	t.mu.Unlock()
	return nil
}

// SetMarginMode records cross vs isolated. V1 always uses isolated math for
// liquidation price; cross is stored for display/parity but does not (yet) widen
// the liquidation buffer using account-wide equity.
func (t *Trader) SetMarginMode(symbol string, isCrossMargin bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.isCrossMargin = isCrossMargin
	t.persistOrLog("set_margin_mode")
	return nil
}

// GetMarketPrice returns the current mark price for a symbol.
func (t *Trader) GetMarketPrice(symbol string) (float64, error) {
	return t.getMarkPrice(symbol)
}

// SetStopLoss stores a stop-loss conditional order. V1 does NOT auto-trigger;
// the framework risk layer must call CloseLong/CloseShort when fired.
func (t *Trader) SetStopLoss(symbol, positionSide string, quantity, stopPrice float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	side := "SELL"
	if positionSide == "SHORT" {
		side = "BUY"
	}
	id := t.nextOrderID("SL")
	t.orders[id] = &pendingOrder{
		id:           id,
		symbol:       symbol,
		side:         side,
		positionSide: positionSide,
		kind:         orderStopLoss,
		stopPrice:    stopPrice,
		quantity:     quantity,
		createdAt:    time.Now().UTC(),
	}
	t.persistOrLog("set_stop_loss")
	return nil
}

// SetTakeProfit stores a take-profit conditional order. V1 does NOT auto-trigger.
func (t *Trader) SetTakeProfit(symbol, positionSide string, quantity, takeProfitPrice float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	side := "SELL"
	if positionSide == "SHORT" {
		side = "BUY"
	}
	id := t.nextOrderID("TP")
	t.orders[id] = &pendingOrder{
		id:           id,
		symbol:       symbol,
		side:         side,
		positionSide: positionSide,
		kind:         orderTakeProfit,
		stopPrice:    takeProfitPrice,
		quantity:     quantity,
		createdAt:    time.Now().UTC(),
	}
	t.persistOrLog("set_take_profit")
	return nil
}

// CancelStopLossOrders removes only stop-loss orders for a symbol.
func (t *Trader) CancelStopLossOrders(symbol string) error {
	return t.cancelByKind(symbol, orderStopLoss)
}

// CancelTakeProfitOrders removes only take-profit orders for a symbol.
func (t *Trader) CancelTakeProfitOrders(symbol string) error {
	return t.cancelByKind(symbol, orderTakeProfit)
}

// CancelStopOrders removes both stop-loss AND take-profit orders for a symbol.
func (t *Trader) CancelStopOrders(symbol string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, o := range t.orders {
		if o.symbol == symbol && (o.kind == orderStopLoss || o.kind == orderTakeProfit) {
			delete(t.orders, id)
		}
	}
	t.persistOrLog("cancel_stop_orders")
	return nil
}

// CancelAllOrders removes every pending order for a symbol.
func (t *Trader) CancelAllOrders(symbol string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, o := range t.orders {
		if o.symbol == symbol {
			delete(t.orders, id)
		}
	}
	t.persistOrLog("cancel_all_orders")
	return nil
}

func (t *Trader) cancelByKind(symbol string, kind orderKind) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, o := range t.orders {
		if o.symbol == symbol && o.kind == kind {
			delete(t.orders, id)
		}
	}
	t.persistOrLog("cancel_by_kind")
	return nil
}

// dropAssociatedOrders is called after closing a position; assumes lock is held.
func (t *Trader) dropAssociatedOrders(symbol string) {
	for id, o := range t.orders {
		if o.symbol == symbol {
			delete(t.orders, id)
		}
	}
}

// FormatQuantity rounds to 6dp; the prompt rules constrain real precision elsewhere.
func (t *Trader) FormatQuantity(symbol string, quantity float64) (string, error) {
	if quantity <= 0 {
		return "0", fmt.Errorf("paper: quantity must be > 0")
	}
	return fmt.Sprintf("%.6f", quantity), nil
}

// GetOrderStatus reports order status for a paper order ID.
//   - market open/close orders fill instantly so we report FILLED via closed history;
//   - pending stop/TP orders report NEW;
//   - unknown IDs return CANCELED to keep the caller flow unblocked.
func (t *Trader) GetOrderStatus(symbol, orderID string) (map[string]interface{}, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if o, ok := t.orders[orderID]; ok {
		return map[string]interface{}{
			"status":      "NEW",
			"symbol":      o.symbol,
			"avgPrice":    0.0,
			"executedQty": 0.0,
			"commission":  0.0,
		}, nil
	}
	for _, rec := range t.closed {
		if rec.OrderID == orderID {
			return map[string]interface{}{
				"status":      "FILLED",
				"symbol":      rec.Symbol,
				"avgPrice":    rec.ExitPrice,
				"executedQty": rec.Quantity,
				"commission":  rec.Fee,
			}, nil
		}
	}
	return map[string]interface{}{
		"status":      "CANCELED",
		"symbol":      symbol,
		"avgPrice":    0.0,
		"executedQty": 0.0,
		"commission":  0.0,
	}, nil
}

// GetClosedPnL returns realized close records since startTime, capped to limit.
func (t *Trader) GetClosedPnL(startTime time.Time, limit int) ([]types.ClosedPnLRecord, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}
	out := make([]types.ClosedPnLRecord, 0, len(t.closed))
	for i := len(t.closed) - 1; i >= 0 && len(out) < limit; i-- {
		rec := t.closed[i]
		if !startTime.IsZero() && rec.ExitTime.Before(startTime) {
			break
		}
		out = append(out, rec)
	}
	return out, nil
}

// GetOpenOrders returns currently pending paper orders for a symbol (or all if "").
func (t *Trader) GetOpenOrders(symbol string) ([]types.OpenOrder, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]types.OpenOrder, 0, len(t.orders))
	for _, o := range t.orders {
		if symbol != "" && o.symbol != symbol {
			continue
		}
		out = append(out, types.OpenOrder{
			OrderID:      o.id,
			Symbol:       o.symbol,
			Side:         o.side,
			PositionSide: o.positionSide,
			Type:         string(o.kind),
			Price:        o.price,
			StopPrice:    o.stopPrice,
			Quantity:     o.quantity,
			Status:       "NEW",
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func unrealizedPnL(p *Position, mark float64) float64 {
	if p == nil || p.Quantity == 0 {
		return 0
	}
	if p.Side == "long" {
		return (mark - p.EntryPrice) * p.Quantity
	}
	return (p.EntryPrice - mark) * p.Quantity
}

func realizedPnL(p *Position, exit float64, qty float64) float64 {
	if p == nil || qty == 0 {
		return 0
	}
	if p.Side == "long" {
		return (exit - p.EntryPrice) * qty
	}
	return (p.EntryPrice - exit) * qty
}

// positionMargin returns the initial margin locked by a position — notional / leverage.
func positionMargin(p *Position) float64 {
	if p == nil || p.Leverage <= 0 {
		return 0
	}
	return (p.EntryPrice * p.Quantity) / float64(p.Leverage)
}

// liquidationPrice returns the isolated-margin liquidation price for a perp
// position, accounting for the maintenance-margin haircut. The long/short
// formulas mirror what real linear-USDT venues publish:
//
//	long  liq = entry * (1 - 1/leverage + maintenance_rate)
//	short liq = entry * (1 + 1/leverage - maintenance_rate)
//
// V1 ignores funding accrual and tier-based maintenance margin.
func liquidationPrice(p *Position) float64 {
	if p == nil || p.Leverage <= 0 {
		return 0
	}
	imBuffer := 1.0 / float64(p.Leverage)
	if p.Side == "long" {
		return p.EntryPrice * (1 - imBuffer + MaintenanceMarginRate)
	}
	return p.EntryPrice * (1 + imBuffer - MaintenanceMarginRate)
}

// isLiquidated reports whether a position is at or beyond its liquidation price
// at the given mark.
func isLiquidated(p *Position, mark float64) bool {
	if p == nil || p.Quantity == 0 {
		return false
	}
	liq := liquidationPrice(p)
	if p.Side == "long" {
		return mark <= liq
	}
	return mark >= liq
}

// totalMarginLockedLocked sums initial margin across all open positions.
// Caller MUST hold t.mu (read or write).
func (t *Trader) totalMarginLockedLocked() float64 {
	sum := 0.0
	for _, p := range t.positions {
		sum += positionMargin(p)
	}
	return sum
}

// tickLocked is the single entry point for periodic state advancement. It
// applies funding (if a funding boundary has elapsed), settles liquidations,
// then triggers stop-loss and take-profit orders. Every Get* and Open path
// calls this first so the caller sees up-to-date state. Caller MUST hold
// t.mu (write). Persists once at end if anything changed.
func (t *Trader) tickLocked() {
	changed := false
	if t.applyFundingLocked() {
		changed = true
	}
	if t.settleLiquidationsLocked() {
		changed = true
	}
	if t.triggerStopOrdersLocked() {
		changed = true
	}
	if changed {
		t.persistOrLog("tick")
	}
}

// nextFundingBoundary returns the next funding settlement time at or after t.
// 8h cycles align to UTC midnight (00:00, 08:00, 16:00) — same convention as
// Binance / Bybit / OKX USDT-M perps.
func nextFundingBoundary(t time.Time, intervalHours int) time.Time {
	t = t.UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	for h := 0; h <= 24; h += intervalHours {
		boundary := dayStart.Add(time.Duration(h) * time.Hour)
		if !boundary.Before(t) {
			return boundary
		}
	}
	return dayStart.Add(24 * time.Hour) // unreachable in normal usage
}

// applyFundingLocked charges/credits funding to every open position once per
// funding cycle. Convention: positive funding rate → longs pay shorts, so for
// a long position the wallet is debited `notional × rate`, for a short it's
// credited. Multiple missed cycles (e.g. paper trader was down for a day) are
// applied in sequence so PnL stays continuous across restarts. Caller MUST
// hold t.mu (write).
func (t *Trader) applyFundingLocked() (changed bool) {
	if len(t.positions) == 0 {
		// Still update the cursor so we don't apply funding retroactively when
		// a position opens later.
		now := t.now()
		if t.lastFundingTime.IsZero() {
			t.lastFundingTime = nextFundingBoundary(now, FundingIntervalHours).Add(-time.Duration(FundingIntervalHours) * time.Hour)
		}
		return false
	}
	now := t.now()
	if t.lastFundingTime.IsZero() {
		// First-ever tick with positions: anchor at the previous boundary so
		// the next crossed boundary triggers a real charge.
		t.lastFundingTime = nextFundingBoundary(now, FundingIntervalHours).Add(-time.Duration(FundingIntervalHours) * time.Hour)
	}

	cursor := t.lastFundingTime
	for {
		boundary := nextFundingBoundary(cursor.Add(time.Nanosecond), FundingIntervalHours)
		if boundary.After(now) {
			break
		}
		// Apply funding for every open position at this boundary.
		for sym, p := range t.positions {
			rate, err := t.getFundingRate(sym)
			if err != nil || rate == 0 {
				continue
			}
			notional := p.Quantity * p.EntryPrice
			payment := notional * rate
			// long with positive rate → pays (debit). short with positive rate → receives (credit).
			if p.Side == "long" {
				t.balance -= payment
			} else {
				t.balance += payment
			}
			changed = true
			logger.Infof("📄 [paper] FUNDING %s %s rate=%.6f payment=%.6f balance=%.2f at %s",
				strings.ToUpper(p.Side), sym, rate, payment, t.balance, boundary.Format(time.RFC3339))
		}
		t.lastFundingTime = boundary
		cursor = boundary
	}
	return changed
}

// triggerStopOrdersLocked closes any position whose mark price has crossed a
// stop-loss or take-profit trigger. Fills happen at the trigger price (V1
// simplification — real exchanges fill at the next available market price,
// which can differ during fast moves). Returns true if any order fired.
func (t *Trader) triggerStopOrdersLocked() (changed bool) {
	type fire struct {
		orderID  string
		symbol   string
		side     string // long / short of the position being closed
		triggerP float64
		kind     orderKind
	}
	var fires []fire

	for id, o := range t.orders {
		if o.kind != orderStopLoss && o.kind != orderTakeProfit {
			continue
		}
		pos, ok := t.positions[o.symbol]
		if !ok {
			// Stale order without an open position — drop it.
			delete(t.orders, id)
			changed = true
			continue
		}
		mark, err := t.getMarkPrice(o.symbol)
		if err != nil {
			continue
		}
		if !shouldTrigger(o, pos.Side, mark) {
			continue
		}
		fires = append(fires, fire{
			orderID: id, symbol: o.symbol, side: pos.Side, triggerP: o.stopPrice, kind: o.kind,
		})
	}

	for _, f := range fires {
		pos, ok := t.positions[f.symbol]
		if !ok {
			continue
		}
		realized := realizedPnL(pos, f.triggerP, pos.Quantity)
		notional := pos.Quantity * f.triggerP
		fee := notional * (t.feeBps / 10000.0)
		t.balance += realized - fee

		closeType := "stop_loss"
		if f.kind == orderTakeProfit {
			closeType = "take_profit"
		}
		t.closed = append(t.closed, types.ClosedPnLRecord{
			Symbol:      f.symbol,
			Side:        f.side,
			EntryPrice:  pos.EntryPrice,
			ExitPrice:   f.triggerP,
			Quantity:    pos.Quantity,
			RealizedPnL: realized - fee,
			Fee:         fee,
			Leverage:    pos.Leverage,
			EntryTime:   pos.OpenedAt,
			ExitTime:    t.now(),
			OrderID:     t.nextOrderID("TRIG"),
			CloseType:   closeType,
			ExchangeID:  "paper",
		})
		// Drop the position and ALL its orders (TP cancelled when SL hits and vice versa).
		delete(t.positions, f.symbol)
		for id, o := range t.orders {
			if o.symbol == f.symbol {
				delete(t.orders, id)
			}
		}
		changed = true
		logger.Infof("📄 [paper] %s %s %s qty=%.6f trigger=%.4f realized=%.4f balance=%.2f",
			strings.ToUpper(closeType), strings.ToUpper(f.side), f.symbol, pos.Quantity, f.triggerP, realized, t.balance)
	}
	return changed
}

// shouldTrigger reports whether a pending stop-loss / take-profit order should
// fire given the current mark and the side of the position protecting it.
//
// Stop-loss is the protective floor (long) or ceiling (short):
//   - long  SL: fire when mark ≤ stopPrice
//   - short SL: fire when mark ≥ stopPrice
//
// Take-profit is the opposite: long TP fires when mark rises past it; short TP
// fires when mark falls past it.
func shouldTrigger(o *pendingOrder, posSide string, mark float64) bool {
	switch {
	case o.kind == orderStopLoss && posSide == "long":
		return mark <= o.stopPrice
	case o.kind == orderStopLoss && posSide == "short":
		return mark >= o.stopPrice
	case o.kind == orderTakeProfit && posSide == "long":
		return mark >= o.stopPrice
	case o.kind == orderTakeProfit && posSide == "short":
		return mark <= o.stopPrice
	default:
		return false
	}
}

// settleLiquidationsLocked force-closes any position whose mark price has
// crossed its liquidation level. The full initial margin is written off (no
// "remainder returned to wallet" — V1 simplification consistent with isolated
// liquidation losing the entire margin). Caller MUST hold t.mu (write).
// Returns true if any position was liquidated.
func (t *Trader) settleLiquidationsLocked() (changed bool) {
	for sym, p := range t.positions {
		mark, err := t.getMarkPrice(sym)
		if err != nil {
			continue // can't decide without a price; leave for next tick
		}
		if !isLiquidated(p, mark) {
			continue
		}
		liq := liquidationPrice(p)
		margin := positionMargin(p)

		// Realized loss = -margin. Wallet absorbs that loss directly.
		t.balance -= margin

		rec := types.ClosedPnLRecord{
			Symbol:      sym,
			Side:        p.Side,
			EntryPrice:  p.EntryPrice,
			ExitPrice:   liq,
			Quantity:    p.Quantity,
			RealizedPnL: -margin,
			Fee:         0,
			Leverage:    p.Leverage,
			EntryTime:   p.OpenedAt,
			ExitTime:    time.Now().UTC(),
			OrderID:     t.nextOrderID("LIQ"),
			CloseType:   "liquidation",
			ExchangeID:  "paper",
		}
		t.closed = append(t.closed, rec)

		for id, o := range t.orders {
			if o.symbol == sym {
				delete(t.orders, id)
			}
		}
		delete(t.positions, sym)

		logger.Infof("📄 [paper] LIQUIDATED %s %s qty=%.6f mark=%.4f liq=%.4f loss=%.4f balance=%.2f",
			strings.ToUpper(p.Side), sym, p.Quantity, mark, liq, margin, t.balance)
		changed = true
	}
	return changed
}

// Time helper — exposed so tests can confirm the funding clock.
func (t *Trader) lastFundingTimeUnsafe() time.Time { return t.lastFundingTime }

