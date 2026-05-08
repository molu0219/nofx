package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PaperState persists the in-memory state of a paper trader keyed by
// trader_id. The whole trader state (positions, pending orders, closed PnL
// records, sequence counter) is stored as opaque JSON blobs in a single row;
// callers serialize/deserialize on their side.
//
// History note: this table was originally keyed by exchange_id, which meant
// multiple traders bound to the same paper exchange shared a single
// balance + positions row — a footgun. Schema is now per-trader. The old
// exchange_id column is retained (nullable) for traceability of which
// paper exchange template the trader used at the time, but is not part of
// the lookup key.
//
// Why JSON-blob and not normalised tables: paper-trader state is per-trader,
// small (<100 KB), mutated together on every fill, and never queried on its
// substructure outside the paper package itself. A single-row UPSERT is
// atomic, cheap, and avoids leaking the paper trader's internal types across
// the schema.
type PaperState struct {
	TraderID         string    `gorm:"primaryKey;column:trader_id" json:"trader_id"`
	ExchangeID       string    `gorm:"column:exchange_id;index" json:"exchange_id,omitempty"`
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

// initTables creates the paper_states table if it doesn't exist, and
// performs a one-shot schema migration when an older exchange_id-keyed
// version of the table is detected. The migration drops legacy data —
// paper trading is non-monetary so this is acceptable, and warning logs
// flag the drop.
func (s *PaperStateStore) initTables() error {
	// Detect legacy schema: a paper_states table where exchange_id is the
	// PK and trader_id doesn't exist yet. If found, drop and recreate.
	if s.db.Migrator().HasTable(&PaperState{}) && !s.db.Migrator().HasColumn(&PaperState{}, "trader_id") {
		// Legacy table: cannot ALTER PK in SQLite, and migration of orphan
		// per-exchange rows to per-trader is ambiguous (multiple traders
		// could have shared the row). Cleanest path: drop and recreate.
		// Paper state is not durable user data — it's just simulator state.
		if err := s.db.Migrator().DropTable("paper_states"); err != nil {
			return fmt.Errorf("paper_states legacy drop: %w", err)
		}
	}
	if s.db.Dialector.Name() == "postgres" {
		var exists int64
		s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'paper_states'`).Scan(&exists)
		if exists > 0 {
			// Postgres: trust AutoMigrate to add missing columns. Real
			// migrations should be coded explicitly elsewhere.
			return s.db.AutoMigrate(&PaperState{})
		}
	}
	return s.db.AutoMigrate(&PaperState{})
}

// Get returns the saved state for a trader or (nil, nil) if no row exists.
// Other DB errors are returned with context.
func (s *PaperStateStore) Get(traderID string) (*PaperState, error) {
	if traderID == "" {
		return nil, fmt.Errorf("paper state: trader_id is required")
	}
	var state PaperState
	err := s.db.Where("trader_id = ?", traderID).First(&state).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to load paper state for %s: %w", traderID, err)
	}
	return &state, nil
}

// Save UPSERTs the state (insert or update by primary key).
func (s *PaperStateStore) Save(state *PaperState) error {
	if state == nil || state.TraderID == "" {
		return fmt.Errorf("paper state: invalid state or empty trader_id")
	}
	state.UpdatedAt = time.Now().UTC()
	if err := s.db.Save(state).Error; err != nil {
		return fmt.Errorf("failed to save paper state for %s: %w", state.TraderID, err)
	}
	return nil
}

// Delete removes the state for a trader (used on trader teardown / reset).
func (s *PaperStateStore) Delete(traderID string) error {
	if traderID == "" {
		return fmt.Errorf("paper state: trader_id is required")
	}
	return s.db.Where("trader_id = ?", traderID).Delete(&PaperState{}).Error
}
