package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pgEntry is the GORM row for one ledger entry - the Postgres LedgerStore's
// on-disk shape for the V4 §4.8 WAL envelope. Payload is jsonb so
// ReadEntries and the OTel exporter's raw lines (kind "otel") round-trip
// arbitrary JSON without a schema per kind.
type pgEntry struct {
	ID      uint      `gorm:"primaryKey;autoIncrement"`
	ChatID  string    `gorm:"column:chat_id;index:idx_ledger_chat_seq,unique,priority:1"`
	Seq     int64     `gorm:"column:seq;index:idx_ledger_chat_seq,unique,priority:2"`
	TurnID  string    `gorm:"column:turn_id"`
	NodeID  string    `gorm:"column:node_id"`
	Kind    string    `gorm:"column:kind"`
	Key     string    `gorm:"column:key"`
	At      time.Time `gorm:"column:at"`
	Payload string    `gorm:"column:payload;type:jsonb"`
}

func (pgEntry) TableName() string { return "ledger_entries" }

// otelEntryKind marks a row written through the plain Append path (the OTel
// exporter's raw lines), as opposed to a typed AppendIntent entry.
const otelEntryKind = "otel"

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
// List/Delete operate at the chat (session) grain like FSStore.
type PGStore struct {
	db *gorm.DB
}

var (
	_ LedgerStore = (*PGStore)(nil)
	_ LedgerStore = (*FSStore)(nil)
)

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
// the same database, but is wired independently of internal/store.
func NewPGStoreFromURL(url string) (*PGStore, error) {
	gormCfg := &gorm.Config{Logger: logger.New(
		slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		logger.Config{SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true},
	)}
	db, err := gorm.Open(postgres.Open(url), gormCfg)
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

// Append stores entry (the OTel exporter's raw JSON line) as kind "otel" -
// the same gapless seq path as AppendIntent, so both kinds of writer share
// one ordering per chat.
func (s *PGStore) Append(ctx context.Context, sessionID string, entry []byte) error {
	_, err := s.appendRow(ctx, pgEntry{
		ChatID:  sessionID,
		Kind:    otelEntryKind,
		At:      time.Now().UTC(),
		Payload: string(entry),
	})
	return err
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
		ChatID: e.ChatID, TurnID: e.TurnID, NodeID: e.NodeID,
		Kind: e.Kind, Key: e.Key, At: e.At, Payload: string(payload),
	})
}

// ReadEntries returns every row for chatID (both Append and AppendIntent
// writers share the same table) with Seq >= fromSeq, in seq order.
func (s *PGStore) ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq >= ?", chatID, fromSeq).
		Order("seq asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read entries for chat %q: %w", chatID, err)
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = Entry{
			Seq: r.Seq, ChatID: r.ChatID, TurnID: r.TurnID, NodeID: r.NodeID,
			Kind: r.Kind, Key: r.Key, At: r.At, Payload: json.RawMessage(r.Payload),
		}
	}
	return out, nil
}

// ReadStream reproduces the JSONL shape FSStore.ReadStream returns: every
// row's payload, one per line, in seq (append) order - so the OTel
// exporter's bundle/export path (internal/ledger.AssembleBundle) is
// unchanged by which LedgerStore backs it.
func (s *PGStore) ReadStream(ctx context.Context, sessionID string) (io.ReadCloser, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ?", sessionID).
		Order("seq asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read stream for chat %q: %w", sessionID, err)
	}
	var buf bytes.Buffer
	for _, r := range rows {
		buf.WriteString(r.Payload)
		buf.WriteByte('\n')
	}
	return io.NopCloser(&buf), nil
}

// List returns one SessionRef per distinct chat_id. Size counts rows, not
// bytes - the FSStore notion of "file size" has no Postgres equivalent, and
// row count serves the same "how big is this session" purpose in practice.
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
