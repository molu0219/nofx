package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PaperState persists the in-memory state of a paper trader keyed by exchange
// account id. The whole trader state (positions, pending orders, closed PnL
// records, sequence counter) is stored as opaque JSON blobs in a single row;
// callers serialize/deserialize on their side.
//
// Why JSON-blob and not normalised tables: paper-trader state is per-exchange,
// small (<100 KB), mutated together on every fill, and never queried on its
// substructure outside the paper package itself. A single-row UPSERT is
// atomic, cheap, and avoids leaking the paper trader's internal types across
// the schema.
type PaperState struct {
	ExchangeID       string    `gorm:"primaryKey;column:exchange_id" json:"exchange_id"`
	Balance          float64   `gorm:"not null;default:0" json:"balance"`
	IsCrossMargin    bool      `gorm:"column:is_cross_margin;default:false" json:"is_cross_margin"`
	OrderSeq         uint64    `gorm:"column:order_seq;default:0" json:"order_seq"`
	LastFundingTime  time.Time `gorm:"column:last_funding_time" json:"last_funding_time"`
	PositionsJSON    string    `gorm:"type:text;column:positions_json" json:"positions_json"`
	OrdersJSON       string    `gorm:"type:text;column:orders_json" json:"orders_json"`
	ClosedJSON       string    `gorm:"type:text;column:closed_json" json:"closed_json"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TableName overrides GORM's pluralization to keep table naming consistent.
func (PaperState) TableName() string { return "paper_states" }

// PaperStateStore is the persistence boundary for paper trader state.
type PaperStateStore struct {
	db *gorm.DB
}

// NewPaperStateStore constructs a store from a GORM handle.
func NewPaperStateStore(db *gorm.DB) *PaperStateStore {
	return &PaperStateStore{db: db}
}

// initTables creates the paper_states table if it doesn't exist.
func (s *PaperStateStore) initTables() error {
	if s.db.Dialector.Name() == "postgres" {
		var exists int64
		s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'paper_states'`).Scan(&exists)
		if exists > 0 {
			return nil
		}
	}
	return s.db.AutoMigrate(&PaperState{})
}

// Get returns the saved state for an exchange or (nil, nil) if no row exists.
// Other DB errors are returned with context.
func (s *PaperStateStore) Get(exchangeID string) (*PaperState, error) {
	if exchangeID == "" {
		return nil, fmt.Errorf("paper state: exchange_id is required")
	}
	var state PaperState
	err := s.db.Where("exchange_id = ?", exchangeID).First(&state).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to load paper state for %s: %w", exchangeID, err)
	}
	return &state, nil
}

// Save UPSERTs the state (insert or update by primary key).
func (s *PaperStateStore) Save(state *PaperState) error {
	if state == nil || state.ExchangeID == "" {
		return fmt.Errorf("paper state: invalid state or empty exchange_id")
	}
	state.UpdatedAt = time.Now().UTC()
	// Save() upserts when the primary key is set.
	if err := s.db.Save(state).Error; err != nil {
		return fmt.Errorf("failed to save paper state for %s: %w", state.ExchangeID, err)
	}
	return nil
}

// Delete removes the state for an exchange (used on trader teardown / reset).
func (s *PaperStateStore) Delete(exchangeID string) error {
	if exchangeID == "" {
		return fmt.Errorf("paper state: exchange_id is required")
	}
	return s.db.Where("exchange_id = ?", exchangeID).Delete(&PaperState{}).Error
}
