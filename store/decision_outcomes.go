package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// DecisionOutcome is one row joining a decision_records entry with the
// strategy version it ran under and the realized PnL of any position that
// was opened by that decision and has since closed. Used by the
// /api/traders/:id/decision-outcomes diagnostic endpoint.
//
// Semantics:
//   - DecisionID + Timestamp + CycleNumber: identity of the decision
//   - StrategyVersionNum: which version of the strategy was active
//     (0 = unstamped — decision was made before audit was wired)
//   - PositionID: 0 when the decision didn't open a position (WAIT, CLOSE,
//     or OPEN that never executed)
//   - RealizedPnL: only meaningful when PositionID > 0 AND PositionStatus
//     == "CLOSED". Open positions show 0.
//   - DurationMin: minutes between EntryTime and ExitTime; 0 for non-closed.
//
// Designed for the diagnostic page, NOT for high-frequency querying — the
// joins are not particularly cheap on large datasets, but with realistic
// trader volumes (~hundreds of decisions/day) it's fine.
type DecisionOutcome struct {
	DecisionID         int64     `json:"decision_id"`
	CycleNumber        int       `json:"cycle_number"`
	Timestamp          time.Time `json:"timestamp"`
	Success            bool      `json:"success"`
	StrategyVersionNum int       `json:"strategy_version_num"`
	StrategyChangeSrc  string    `json:"strategy_change_source"`
	PositionID         int64     `json:"position_id,omitempty"`
	Symbol             string    `json:"symbol,omitempty"`
	Side               string    `json:"side,omitempty"`
	EntryPrice         float64   `json:"entry_price,omitempty"`
	ExitPrice          float64   `json:"exit_price,omitempty"`
	RealizedPnL        float64   `json:"realized_pnl"`
	PositionStatus     string    `json:"position_status,omitempty"`
	CloseReason        string    `json:"close_reason,omitempty"`
	DurationMin        float64   `json:"duration_min,omitempty"`
}

// DecisionOutcomesStore reads the joined decision/strategy/position view.
type DecisionOutcomesStore struct {
	db *gorm.DB
}

// NewDecisionOutcomesStore constructs the store.
func NewDecisionOutcomesStore(db *gorm.DB) *DecisionOutcomesStore {
	return &DecisionOutcomesStore{db: db}
}

// ListForTrader returns the most recent N decisions for a trader, each
// joined with its strategy version and any position it opened. Ordered
// newest-first.
//
// The LEFT JOIN on trader_positions intentionally handles "decision didn't
// open anything" — most cycles produce no trades, so PositionID will be 0
// for those rows. UI should treat zero as "no position" not "missing data".
func (s *DecisionOutcomesStore) ListForTrader(traderID string, limit int) ([]DecisionOutcome, error) {
	if traderID == "" {
		return nil, fmt.Errorf("decision outcomes: trader_id is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rawSQL := `
		SELECT
			dr.id AS decision_id,
			dr.cycle_number,
			dr.timestamp,
			dr.success,
			COALESCE(sv.version_num, 0) AS strategy_version_num,
			COALESCE(sv.change_source, '') AS strategy_change_src,
			COALESCE(p.id, 0) AS position_id,
			COALESCE(p.symbol, '') AS symbol,
			COALESCE(p.side, '') AS side,
			COALESCE(p.entry_price, 0) AS entry_price,
			COALESCE(p.exit_price, 0) AS exit_price,
			COALESCE(p.realized_pnl, 0) AS realized_pnl,
			COALESCE(p.status, '') AS position_status,
			COALESCE(p.close_reason, '') AS close_reason,
			CASE
				WHEN p.exit_time > 0 AND p.entry_time > 0
					THEN CAST((p.exit_time - p.entry_time) AS REAL) / 60000
				ELSE 0
			END AS duration_min
		FROM decision_records dr
		LEFT JOIN strategy_versions sv ON sv.id = dr.strategy_version_id
		LEFT JOIN trader_positions p ON p.opened_by_decision_id = dr.id
		WHERE dr.trader_id = ?
		ORDER BY dr.timestamp DESC
		LIMIT ?`

	rows := []DecisionOutcome{}
	if err := s.db.Raw(rawSQL, traderID, limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("decision outcomes: query: %w", err)
	}
	return rows, nil
}

// VersionTimeline returns strategy version events for a trader, ordered
// oldest-first so a UI can render them as a horizontal timeline overlaid
// on the decision/equity chart.
func (s *DecisionOutcomesStore) VersionTimeline(traderID string, limit int) ([]StrategyVersion, error) {
	if traderID == "" {
		return nil, fmt.Errorf("version timeline: trader_id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var strategyID string
	if err := s.db.Raw("SELECT strategy_id FROM traders WHERE id = ?", traderID).
		Scan(&strategyID).Error; err != nil {
		return nil, fmt.Errorf("version timeline: read strategy id: %w", err)
	}
	if strategyID == "" {
		return nil, nil
	}
	var out []StrategyVersion
	if err := s.db.
		Where("strategy_id = ?", strategyID).
		Order("version_num ASC").
		Limit(limit).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("version timeline: list: %w", err)
	}
	return out, nil
}
