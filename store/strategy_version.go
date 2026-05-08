package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// StrategyVersion is an append-only audit row capturing one revision of a
// strategy's config. Whenever the optimizer (or a human) mutates a
// strategy, a new row goes into this table — the live `strategies.config`
// blob is also updated, but historical state is preserved here.
//
// The optimizer feedback loop reads recent versions for a trader to ground
// its next change in "what we already tried + what happened after". The
// diagnostic UI reads this to overlay strategy-change events onto the
// equity / decision-outcome timeline.
//
// Schema notes:
//   - VersionNum is per-strategy monotonically increasing. Computed as
//     max(version_num)+1 inside Append().
//   - ChangeSource is "optimizer" | "user" | "system" — read by analytics
//     to bucket changes by responsible party.
//   - Reasoning is free-form (the optimizer LLM's stated rationale, or a
//     UI-edit changelog); intentionally not enum'd.
//   - ConfigJSON is the full marshalled config at time of write — same
//     blob shape as strategies.config. Larger storage cost than a diff,
//     but trivially queryable + lets us trivially "rollback to v3".
type StrategyVersion struct {
	ID           uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	StrategyID   string    `gorm:"index;column:strategy_id" json:"strategy_id"`
	VersionNum   int       `gorm:"column:version_num" json:"version_num"`
	ConfigJSON   string    `gorm:"type:text;column:config_json" json:"config_json"`
	ChangeSource string    `gorm:"column:change_source" json:"change_source"`
	Reasoning    string    `gorm:"type:text;column:reasoning" json:"reasoning"`
	CreatedAt    time.Time `gorm:"index" json:"created_at"`
}

// TableName overrides default pluralization.
func (StrategyVersion) TableName() string { return "strategy_versions" }

// StrategyVersionStore is the persistence boundary for the audit table.
type StrategyVersionStore struct {
	db *gorm.DB
}

// NewStrategyVersionStore constructs a store from a GORM handle.
func NewStrategyVersionStore(db *gorm.DB) *StrategyVersionStore {
	return &StrategyVersionStore{db: db}
}

func (s *StrategyVersionStore) initTables() error {
	return s.db.AutoMigrate(&StrategyVersion{})
}

// Append writes a new version row. VersionNum is auto-assigned as the next
// integer for this strategy_id. Returns the assigned version number so the
// caller (e.g. AutoTrader.recordDecision) can stamp it onto the decision.
func (s *StrategyVersionStore) Append(strategyID, configJSON, source, reasoning string) (int, error) {
	if strategyID == "" {
		return 0, fmt.Errorf("strategy version: strategy_id is required")
	}
	var maxV int
	if err := s.db.Model(&StrategyVersion{}).
		Where("strategy_id = ?", strategyID).
		Select("COALESCE(MAX(version_num), 0)").
		Scan(&maxV).Error; err != nil {
		return 0, fmt.Errorf("strategy version: read max version: %w", err)
	}
	v := &StrategyVersion{
		StrategyID:   strategyID,
		VersionNum:   maxV + 1,
		ConfigJSON:   configJSON,
		ChangeSource: source,
		Reasoning:    reasoning,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.db.Create(v).Error; err != nil {
		return 0, fmt.Errorf("strategy version: insert: %w", err)
	}
	return v.VersionNum, nil
}

// Latest returns the most recent version row for a strategy, or (nil, nil)
// if no version was ever recorded (e.g. for a brand-new strategy that
// hasn't gone through Append yet).
func (s *StrategyVersionStore) Latest(strategyID string) (*StrategyVersion, error) {
	var v StrategyVersion
	err := s.db.Where("strategy_id = ?", strategyID).
		Order("version_num DESC").
		First(&v).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("strategy version: latest: %w", err)
	}
	return &v, nil
}

// RecentForTrader returns the last N versions of the strategy bound to the
// given trader, ordered newest-first. Used by the optimizer feedback loop —
// the trader maps to one strategy via traders.strategy_id, and we want
// "what changes have been made to my config recently".
func (s *StrategyVersionStore) RecentForTrader(traderID string, limit int) ([]StrategyVersion, error) {
	if limit <= 0 {
		limit = 5
	}
	// Subquery: which strategy is this trader on?
	var strategyID string
	if err := s.db.Raw(
		"SELECT strategy_id FROM traders WHERE id = ?",
		traderID,
	).Scan(&strategyID).Error; err != nil {
		return nil, fmt.Errorf("strategy version: read trader strategy: %w", err)
	}
	if strategyID == "" {
		return nil, nil
	}
	var out []StrategyVersion
	if err := s.db.
		Where("strategy_id = ?", strategyID).
		Order("version_num DESC").
		Limit(limit).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("strategy version: list: %w", err)
	}
	return out, nil
}

// VersionWithMetrics bundles a strategy version with the realized outcome
// of decisions made under it. Used by the optimizer feedback loop to ground
// "what should I change next" in "what did the last few changes actually
// do to PnL".
type VersionWithMetrics struct {
	Version      StrategyVersion
	Decisions    int     // number of decisions logged under this version
	Trades       int     // positions opened via those decisions (closed or open)
	WinTrades    int     // closed positions with realized_pnl > 0
	LossTrades   int     // closed positions with realized_pnl <= 0
	NetPnL       float64 // sum of realized_pnl across those positions (closed only)
	WinRate      float64 // wins / (wins + losses); 0 if no closed trades
}

// RecentForTraderWithMetrics is RecentForTrader + per-version outcomes.
// Computed via two follow-up queries per version (cheap when limit is small,
// e.g. 5): count decisions and aggregate realized PnL of opened positions.
//
// Per-version outcomes can be partial — open trades show as Trades > Wins+Losses
// because they have NetPnL = 0. That's fine for the optimizer signal:
// "config v6 has 3 open trades, none closed yet, can't judge".
func (s *StrategyVersionStore) RecentForTraderWithMetrics(traderID string, limit int) ([]VersionWithMetrics, error) {
	versions, err := s.RecentForTrader(traderID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]VersionWithMetrics, 0, len(versions))
	for _, v := range versions {
		row := VersionWithMetrics{Version: v}
		// Count decisions made under this version.
		var decisionCount int64
		if err := s.db.Raw(
			"SELECT COUNT(*) FROM decision_records WHERE trader_id = ? AND strategy_version_id = ?",
			traderID, v.ID,
		).Scan(&decisionCount).Error; err != nil {
			return nil, fmt.Errorf("metrics: count decisions: %w", err)
		}
		row.Decisions = int(decisionCount)
		// Aggregate position outcomes opened via these decisions.
		var agg struct {
			Trades  int
			Wins    int
			Losses  int
			NetPnL  float64
		}
		if err := s.db.Raw(`
			SELECT
				COUNT(*) AS trades,
				SUM(CASE WHEN status = 'CLOSED' AND realized_pnl > 0 THEN 1 ELSE 0 END) AS wins,
				SUM(CASE WHEN status = 'CLOSED' AND realized_pnl <= 0 THEN 1 ELSE 0 END) AS losses,
				SUM(CASE WHEN status = 'CLOSED' THEN realized_pnl ELSE 0 END) AS net_pnl
			FROM trader_positions
			WHERE trader_id = ?
				AND opened_by_decision_id IN (
					SELECT id FROM decision_records
					WHERE trader_id = ? AND strategy_version_id = ?
				)`,
			traderID, traderID, v.ID,
		).Scan(&agg).Error; err != nil {
			return nil, fmt.Errorf("metrics: aggregate positions: %w", err)
		}
		row.Trades = agg.Trades
		row.WinTrades = agg.Wins
		row.LossTrades = agg.Losses
		row.NetPnL = agg.NetPnL
		if agg.Wins+agg.Losses > 0 {
			row.WinRate = float64(agg.Wins) / float64(agg.Wins+agg.Losses)
		}
		out = append(out, row)
	}
	return out, nil
}
