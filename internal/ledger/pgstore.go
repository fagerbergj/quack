package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fagerbergj/quack/internal/pgdial"
)

// pgEntry is the GORM row for one ledger Entry. Payload is jsonb so every
// kind round-trips arbitrary JSON without a schema per kind.
type pgEntry struct {
	ID      uint      `gorm:"primaryKey;autoIncrement"`
	ChatID  string    `gorm:"column:chat_id;index:idx_ledger_chat_seq,unique,priority:1;index:idx_ledger_chat_key,priority:1"`
	Seq     int64     `gorm:"column:seq;index:idx_ledger_chat_seq,unique,priority:2"`
	TurnID  string    `gorm:"column:turn_id"`
	NodeID  string    `gorm:"column:node_id"`
	Agent   string    `gorm:"column:agent"`
	Round   string    `gorm:"column:round"`
	Kind    string    `gorm:"column:kind"`
	Key     string    `gorm:"column:key;index:idx_ledger_chat_key,priority:2"`
	At      time.Time `gorm:"column:at"`
	Payload string    `gorm:"column:payload;type:jsonb"`
}

func (pgEntry) TableName() string { return "ledger_entries" }

// pgSeqCounter holds the next seq to allocate for one chat. See nextSeq for
// why a single UPSERT on this row is enough to make allocation race-free.
type pgSeqCounter struct {
	ChatID  string `gorm:"column:chat_id;primaryKey"`
	NextSeq int64  `gorm:"column:next_seq"`
}

func (pgSeqCounter) TableName() string { return "ledger_seq_counters" }

// PGStore is the Postgres LedgerStore adapter (V4 §4.8): the WAL backend,
// meant to run against the same database as internal/store. It is the only
// adapter with a real transactional, gapless per-chat seq (see nextSeq);
// List/Delete operate at the chat grain.
type PGStore struct {
	db *gorm.DB
}

// NewPGStore migrates the ledger's own tables on db and returns a store
// backed by it. db is expected to already point at the app's Postgres
// database (internal/store.New / store.NewArtifactService open their own
// connections the same way).
func NewPGStore(db *gorm.DB) (*PGStore, error) {
	if err := db.AutoMigrate(&pgEntry{}, &pgSeqCounter{}); err != nil {
		return nil, fmt.Errorf("ledger: automigrate postgres store: %w", err)
	}
	return &PGStore{db: db}, nil
}

// NewPGStoreFromURL opens its own Postgres connection at url, mirroring
// internal/store.NewArtifactService - the ledger store is meant to point at
// the same database, but is wired independently of internal/store. Uses
// pgdial.Open so this dialector gets the same dial retry as every other one
// (#1200 review: this was a fourth postgres dialector missed by the first pass).
func NewPGStoreFromURL(url string) (*PGStore, error) {
	gormCfg := &gorm.Config{Logger: logger.New(
		slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		logger.Config{SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true},
	)}
	dialector, err := pgdial.Open(url)
	if err != nil {
		return nil, fmt.Errorf("ledger: parse postgres url: %w", err)
	}
	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		return nil, fmt.Errorf("ledger: open postgres store: %w", err)
	}
	return NewPGStore(db)
}

// nextSeq atomically allocates the next seq for chatID inside tx: one
// UPSERT that either inserts the counter at 1 or increments it, returning
// the new value. Postgres locks the counter row for the UPDATE branch, so N
// concurrent callers serialize on that single row and each gets a distinct,
// gapless value - no explicit advisory lock or SELECT ... FOR UPDATE
// needed, and the increment and the entry insert commit together in tx, so
// a failed insert never leaves the counter incremented without a row.
func nextSeq(tx *gorm.DB, chatID string) (int64, error) {
	var row pgSeqCounter
	err := tx.Raw(`
		INSERT INTO ledger_seq_counters (chat_id, next_seq) VALUES (?, 1)
		ON CONFLICT (chat_id) DO UPDATE SET next_seq = ledger_seq_counters.next_seq + 1
		RETURNING chat_id, next_seq
	`, chatID).Scan(&row).Error
	if err != nil {
		return 0, fmt.Errorf("ledger: allocate seq for chat %q: %w", chatID, err)
	}
	return row.NextSeq, nil
}

// appendRow allocates row's seq and inserts it in one transaction, so seq
// allocation and the row it belongs to are always consistent: a rollback
// (insert failure) never leaves a seq "used" with no row for it.
func (s *PGStore) appendRow(ctx context.Context, row pgEntry) (int64, error) {
	var seq int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		seq, err = nextSeq(tx, row.ChatID)
		if err != nil {
			return err
		}
		row.Seq = seq
		return tx.Create(&row).Error
	})
	if err != nil {
		return 0, fmt.Errorf("ledger: append chat %q: %w", row.ChatID, err)
	}
	return seq, nil
}

// AppendIntent is the fail-closed WAL path: a non-nil error means no row
// was written, so the caller must not perform the state change either.
func (s *PGStore) AppendIntent(ctx context.Context, e Entry) (int64, error) {
	if e.ChatID == "" || e.Kind == "" {
		return 0, fmt.Errorf("ledger: intent needs chat_id and kind")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	payload := e.Payload
	if payload == nil {
		payload = json.RawMessage("null")
	}
	return s.appendRow(ctx, pgEntry{
		ChatID: e.ChatID, TurnID: e.TurnID, NodeID: e.NodeID, Agent: e.Agent, Round: e.Round,
		Kind: e.Kind, Key: e.Key, At: e.At, Payload: string(payload),
	})
}

// ReadEntries returns every row for chatID with Seq >= fromSeq, in seq order.
func (s *PGStore) ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq >= ?", chatID, fromSeq).
		Order("seq asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read entries for chat %q: %w", chatID, err)
	}
	return pgRowsToEntries(rows), nil
}

// ReadEntriesByKey is #1101's fold optimization: recordstore.lastRevision
// (via internal/ledger/fold.LastRevision) needs only one id's entries, and
// the (chat_id, key) index (idx_ledger_chat_key) makes that a server-side
// filter instead of a full per-chat scan. Optional on LedgerStore - a store
// without it is scanned and filtered in memory by the fold.
func (s *PGStore) ReadEntriesByKey(ctx context.Context, chatID, key string, fromSeq int64) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ? AND key = ? AND seq >= ?", chatID, key, fromSeq).
		Order("seq asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read entries for chat %q key %q: %w", chatID, key, err)
	}
	return pgRowsToEntries(rows), nil
}

// ReadEntriesPage is #1101's paging optimization: fold.Fold pages through a
// big chat page-by-page instead of loading it in one slice (ReadEntries's
// contract). Optional on LedgerStore - a store without it is read in one
// ReadEntries call.
func (s *PGStore) ReadEntriesPage(ctx context.Context, chatID string, fromSeq int64, limit int) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq >= ?", chatID, fromSeq).
		Order("seq asc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read entries page for chat %q: %w", chatID, err)
	}
	return pgRowsToEntries(rows), nil
}

func pgRowsToEntries(rows []pgEntry) []Entry {
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = Entry{
			Seq: r.Seq, ChatID: r.ChatID, TurnID: r.TurnID, NodeID: r.NodeID, Agent: r.Agent, Round: r.Round,
			Kind: r.Kind, Key: r.Key, At: r.At, Payload: json.RawMessage(r.Payload),
		}
	}
	return out
}

// List returns one SessionRef per distinct chat_id; Size counts rows.
func (s *PGStore) List(ctx context.Context) ([]SessionRef, error) {
	var aggs []struct {
		ChatID string
		Cnt    int64
		Last   time.Time
	}
	if err := s.db.WithContext(ctx).Model(&pgEntry{}).
		Select("chat_id, count(*) as cnt, max(at) as last").
		Group("chat_id").Scan(&aggs).Error; err != nil {
		return nil, fmt.Errorf("ledger: list sessions: %w", err)
	}
	out := make([]SessionRef, len(aggs))
	for i, a := range aggs {
		out[i] = SessionRef{ID: a.ChatID, Size: a.Cnt, ModTime: a.Last}
	}
	return out, nil
}

// Delete removes a whole chat's entries and its seq counter - a hard delete
// only, never a partial one, per the LedgerStore contract.
func (s *PGStore) Delete(ctx context.Context, sessionID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("chat_id = ?", sessionID).Delete(&pgEntry{}).Error; err != nil {
			return fmt.Errorf("ledger: delete entries for chat %q: %w", sessionID, err)
		}
		if err := tx.Where("chat_id = ?", sessionID).Delete(&pgSeqCounter{}).Error; err != nil {
			return fmt.Errorf("ledger: delete seq counter for chat %q: %w", sessionID, err)
		}
		return nil
	})
}
