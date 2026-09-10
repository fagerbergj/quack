package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fagerbergj/quack/internal/pgdial"
)

var _ LedgerStore = (*PGStore)(nil)

// idxParentRevision/idxIdempotency: named explicitly (not GORM's tag-derived
// naming) so AppendIntent can tell which one a 23505 violation came from.
const (
	idxParentRevision = "idx_ledger_artifact_parent_revision"
	idxIdempotency    = "idx_ledger_idempotency_key"
)

// pgEntry is the GORM row for one ledger Entry; Payload is jsonb. ParentRevision
// comes from an artifact.revision entry's payload (0, ignored, otherwise).
type pgEntry struct {
	ID             uint      `gorm:"primaryKey;autoIncrement"`
	ChatID         string    `gorm:"column:chat_id;index:idx_ledger_chat_seq,unique,priority:1;index:idx_ledger_chat_key,priority:1"`
	Seq            int64     `gorm:"column:seq;index:idx_ledger_chat_seq,unique,priority:2"`
	TurnID         string    `gorm:"column:turn_id"`
	NodeID         string    `gorm:"column:node_id"`
	Agent          string    `gorm:"column:agent"`
	Round          string    `gorm:"column:round"`
	Kind           string    `gorm:"column:kind"`
	Key            string    `gorm:"column:key;index:idx_ledger_chat_key,priority:2"`
	At             time.Time `gorm:"column:at"`
	Payload        string    `gorm:"column:payload;type:jsonb"`
	ParentRevision int64     `gorm:"column:parent_revision"`
	IdempotencyKey string    `gorm:"column:idempotency_key"`
	// SchemaVersion: 0 on every pre-#1144-P5 row - ADD COLUMN never
	// backfills existing rows, and it doesn't need to: pgRowsToEntries runs
	// every row through MigrateEntry, which treats 0 as version 1.
	SchemaVersion int `gorm:"column:schema_version"`
}

func (pgEntry) TableName() string { return "ledger_entries" }

// pgSeqCounter holds the next seq to allocate for one chat. See nextSeq for
// why a single UPSERT on this row is enough to make allocation race-free.
type pgSeqCounter struct {
	ChatID  string `gorm:"column:chat_id;primaryKey"`
	NextSeq int64  `gorm:"column:next_seq"`
}

func (pgSeqCounter) TableName() string { return "ledger_seq_counters" }

// PGStore is the Postgres LedgerStore adapter (V4 §4.8), meant to share the app's
// database with internal/store - the only adapter with a transactional, gapless
// per-chat seq (see nextSeq); List/Delete operate at the chat grain.
type PGStore struct {
	db *gorm.DB
}

// NewPGStore migrates the ledger's tables on db and returns a store backed by
// it; db is expected to already point at the app's Postgres database (internal/
// store.New and store.NewArtifactService open their own connections the same way).
func NewPGStore(db *gorm.DB) (*PGStore, error) {
	if err := db.AutoMigrate(&pgEntry{}, &pgSeqCounter{}); err != nil {
		return nil, fmt.Errorf("ledger: automigrate postgres store: %w", err)
	}
	if err := ensureParentRevisionIndex(db); err != nil {
		return nil, err
	}
	if err := db.Exec(fmt.Sprintf(
		`CREATE UNIQUE INDEX IF NOT EXISTS %s ON ledger_entries (chat_id, idempotency_key) WHERE idempotency_key <> ''`,
		idxIdempotency)).Error; err != nil {
		return nil, fmt.Errorf("ledger: create idempotency index: %w", err)
	}
	return &PGStore{db: db}, nil
}

// ensureParentRevisionIndex adds the unique (chat_id, key, parent_revision) index
// (#1144 P4); a migrated/restored database isn't provably duplicate-free, so this
// checks first and refuses to start rather than create a broken index or leave the guarantee silently unenforced.
func ensureParentRevisionIndex(db *gorm.DB) error {
	var exists int
	if err := db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'ledger_entries' AND indexname = ?`, idxParentRevision).Scan(&exists).Error; err != nil {
		return fmt.Errorf("ledger: check for parent revision index: %w", err)
	}
	if exists > 0 {
		return nil // already created (and therefore already free of duplicates) - skip the full-table scan below on every boot
	}
	// AutoMigrate's ADD COLUMN leaves parent_revision NULL on pre-existing rows (Postgres
	// never backfills); left NULL, the dedup GROUP BY below folds ALL of them together
	// (SQL groups NULLs as equal) and wedges the deploy - backfill from the payload first.
	if err := db.Exec(`
		UPDATE ledger_entries SET parent_revision = (payload->>'parent_revision')::bigint
		WHERE kind = ? AND parent_revision IS NULL
	`, KindArtifactRevision).Error; err != nil {
		return fmt.Errorf("ledger: backfill parent_revision from payload: %w", err)
	}
	var dupes []struct {
		ChatID         string
		Key            string
		ParentRevision int64
		Cnt            int64
	}
	if err := db.Raw(`
		SELECT chat_id, key, parent_revision, count(*) as cnt FROM ledger_entries
		WHERE kind = ? AND parent_revision IS NOT NULL
		GROUP BY chat_id, key, parent_revision HAVING count(*) > 1 LIMIT 10
	`, KindArtifactRevision).Scan(&dupes).Error; err != nil {
		return fmt.Errorf("ledger: check for duplicate parent revisions: %w", err)
	}
	if len(dupes) > 0 {
		d := dupes[0]
		return fmt.Errorf("ledger: %d+ (chat_id,key,parent_revision) duplicate group(s) in ledger_entries "+
			"(e.g. chat=%q key=%q parent_revision=%d x%d) - the #1144 P4 unique index can't be created until "+
			"these are resolved manually; refusing to start with the guarantee silently unenforced",
			len(dupes), d.ChatID, d.Key, d.ParentRevision, d.Cnt)
	}
	return db.Exec(fmt.Sprintf(
		`CREATE UNIQUE INDEX IF NOT EXISTS %s ON ledger_entries (chat_id, key, parent_revision) WHERE kind = '%s'`,
		idxParentRevision, KindArtifactRevision)).Error
}

// NewPGStoreFromURL opens its own Postgres connection at url, mirroring
// internal/store.NewArtifactService (same database, wired independently of
// internal/store). Uses pgdial.Open for the shared dial retry (#1200 review: a fourth dialector missed by the first pass).
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

// nextSeq atomically allocates the next seq for chatID inside tx: one UPSERT (insert at 1
// or increment) whose row lock serializes concurrent callers into distinct, gapless values - no
// advisory lock needed. The increment and the entry insert commit together in tx, so a failed insert never leaves the counter incremented without a row.
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

// AppendIntent is the fail-closed WAL path (#1144 P4): a non-nil error means
// no row was written, including the typed ErrStaleParent/DuplicateIntentError.
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
	// Checked before inserting, not just after: a repeat save must short-circuit even with a
	// now-stale parent (see MemStore's matching comment); a concurrent duplicate can still
	// race past this read, and the insert's idxIdempotency violation below is the fallback.
	if e.IdempotencyKey != "" {
		if existing, ferr := s.findByIdempotencyKey(ctx, e.ChatID, e.IdempotencyKey); ferr == nil {
			return 0, &DuplicateIntentError{Existing: existing}
		}
	}
	seq, err := s.appendRow(ctx, pgEntry{
		ChatID: e.ChatID, TurnID: e.TurnID, NodeID: e.NodeID, Agent: e.Agent, Round: e.Round,
		Kind: e.Kind, Key: e.Key, At: e.At, Payload: string(payload),
		ParentRevision: ParentRevisionOf(e.Kind, payload), IdempotencyKey: e.IdempotencyKey,
		SchemaVersion: EntrySchemaVersion,
	})
	if err != nil {
		switch pgConstraintName(err) {
		case idxParentRevision:
			return 0, ErrStaleParent
		case idxIdempotency:
			existing, ferr := s.findByIdempotencyKey(ctx, e.ChatID, e.IdempotencyKey)
			if ferr != nil {
				return 0, fmt.Errorf("ledger: duplicate idempotency key for chat %q but couldn't read the existing entry: %w", e.ChatID, ferr)
			}
			return 0, &DuplicateIntentError{Existing: existing}
		}
		return 0, err
	}
	return seq, nil
}

// pgConstraintName extracts the violated unique index's name from a
// Postgres 23505 error, "" for anything else (a different error code, or no
// pgconn.PgError in the chain at all).
func pgConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return ""
	}
	return pgErr.ConstraintName
}

// findByIdempotencyKey reads back the entry that already claimed key, for
// AppendIntent to hand the caller as *DuplicateIntentError.Existing.
func (s *PGStore) findByIdempotencyKey(ctx context.Context, chatID, key string) (Entry, error) {
	var row pgEntry
	if err := s.db.WithContext(ctx).Where("chat_id = ? AND idempotency_key = ?", chatID, key).Take(&row).Error; err != nil {
		return Entry{}, fmt.Errorf("ledger: read entry for idempotency key: %w", err)
	}
	return pgRowsToEntries([]pgEntry{row})[0], nil
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

// ReadEntriesFiltered is ReadEntries with `kind IN (?)` pushed to SQL, so
// Postgres never detoasts the payload of a row the caller doesn't want
// (perf audit #1 - kinds like agent.invoke run to multi-MB jsonb).
func (s *PGStore) ReadEntriesFiltered(ctx context.Context, chatID string, fromSeq int64, kinds []string) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("chat_id = ? AND seq >= ? AND kind IN ?", chatID, fromSeq, kinds).
		Order("seq asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read filtered entries for chat %q: %w", chatID, err)
	}
	return pgRowsToEntries(rows), nil
}

// ReadEntriesFilteredSince pushes down ReadAllByKindsSince's cross-chat query: one
// `kind IN (?) AND at >= ?` scan instead of List() plus one ReadEntriesFiltered per chat
// (perf audit #12 - 94 queries and 0.8-5.0s per memory-page load on a 465k-entry ledger).
func (s *PGStore) ReadEntriesFilteredSince(ctx context.Context, kinds []string, since time.Time) ([]Entry, error) {
	var rows []pgEntry
	if err := s.db.WithContext(ctx).
		Where("kind IN ? AND at >= ?", kinds, since).
		Order("chat_id, seq").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: read filtered entries since %s: %w", since, err)
	}
	return pgRowsToEntries(rows), nil
}

// MaxSeq reads chatID's highest allocated seq straight off the seq counter
// row (a PK lookup) instead of MAX(seq) over ledger_entries, 0 if the chat
// never appended.
func (s *PGStore) MaxSeq(ctx context.Context, chatID string) (int64, error) {
	var row pgSeqCounter
	err := s.db.WithContext(ctx).Where("chat_id = ?", chatID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: max seq for chat %q: %w", chatID, err)
	}
	return row.NextSeq, nil
}

// ReadEntriesPage is #1101's paging optimization: fold.Fold pages through a big chat
// instead of loading it in one slice (ReadEntries's contract). Optional on LedgerStore -
// a store without it is read in one ReadEntries call.
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
		out[i] = MigrateEntry(Entry{
			Seq: r.Seq, ChatID: r.ChatID, TurnID: r.TurnID, NodeID: r.NodeID, Agent: r.Agent, Round: r.Round,
			Kind: r.Kind, Key: r.Key, At: r.At, Payload: json.RawMessage(r.Payload), IdempotencyKey: r.IdempotencyKey,
			SchemaVersion: r.SchemaVersion,
		})
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
