package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// Not modernc.org/sqlite directly: quack already registers "sqlite" via glebarez, and a second
	// registration panics at init. glebarez wraps modernc, so it's the same engine.
	_ "github.com/glebarez/go-sqlite"
)

// ghStore is this extension's private SQLite: snapshot, review baseline, CI-fix state, merge intent and
// pending run, each keyed by chat with its own read-modify-write pattern.
type ghStore struct {
	path   string
	mu     sync.Mutex
	db     *sql.DB
	closed bool
}

const ghSchema = `
CREATE TABLE IF NOT EXISTS github_snapshot (
	chat_id TEXT PRIMARY KEY,
	json TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_review_baseline (
	chat_id TEXT PRIMARY KEY,
	patch_ids TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_fix_state (
	chat_id TEXT PRIMARY KEY,
	last_sha TEXT NOT NULL,
	stopped INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_merge_intent (
	chat_id TEXT PRIMARY KEY,
	requested_by TEXT NOT NULL,
	dispatched_head TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_pending_run (
	chat_id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	owner TEXT NOT NULL,
	repo TEXT NOT NULL,
	number INTEGER NOT NULL,
	is_pr INTEGER NOT NULL,
	login TEXT NOT NULL,
	is_plan INTEGER NOT NULL,
	is_label_trigger INTEGER NOT NULL,
	comment_id INTEGER NOT NULL,
	default_branch TEXT NOT NULL,
	installation_id INTEGER NOT NULL,
	clone_url TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL
);
`

// newStore does no I/O: Factories must stay side-effect free (sdk.Factory),
// so the database opens and migrates on first use or in Start.
func newStore(dataDir string) *ghStore {
	return &ghStore{path: filepath.Join(dataDir, "github.sqlite")}
}

// errStoreClosed is returned by every query after Close.
var errStoreClosed = errors.New("github: store closed")

// conn opens and migrates the database on first call; a failed open is retried on the next.
func (s *ghStore) conn() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errStoreClosed
	}
	if s.db == nil {
		db, err := openDB(s.path)
		if err != nil {
			return nil, err
		}
		s.db = db
	}
	return s.db, nil
}

// openDB creates and migrates the database. MaxOpenConns(1) serializes every statement,
// sidestepping SQLITE_BUSY without WAL/busy_timeout tuning (see store_test.go's concurrency test).
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("github: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ghSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("github: migrate store: %w", err)
	}
	// Additive migration for databases predating dispatched_head; "duplicate column" means already applied.
	if _, err := db.Exec(`ALTER TABLE github_merge_intent ADD COLUMN dispatched_head TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("github: migrate store: add dispatched_head: %w", err)
	}
	return db, nil
}

// Close is idempotent; later queries fail with errStoreClosed instead of reopening.
func (s *ghStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.db == nil {
		return nil
	}
	db := s.db
	s.db = nil
	return db.Close()
}

func (s *ghStore) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, query, args...)
}

// row defers a conn() error to Scan, so callers keep the QueryRowContext(...).Scan shape.
type row struct {
	r   *sql.Row
	err error
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.r.Scan(dest...)
}

func (s *ghStore) queryRow(ctx context.Context, query string, args ...any) row {
	db, err := s.conn()
	if err != nil {
		return row{err: err}
	}
	return row{r: db.QueryRowContext(ctx, query, args...)}
}

// getChatString runs a single-string-column query by chat_id, returning (value, found).
func (s *ghStore) getChatString(ctx context.Context, query, chatID string) (string, bool, error) {
	var v string
	err := s.queryRow(ctx, query, chatID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// GetSnapshot returns the stored snapshot JSON, or ("", false, nil) when none exists.
func (s *ghStore) GetSnapshot(ctx context.Context, chatID string) (string, bool, error) {
	return s.getChatString(ctx, `SELECT json FROM github_snapshot WHERE chat_id = ?`, chatID)
}

// SetSnapshot upserts the snapshot JSON for the next resume's diff.
func (s *ghStore) SetSnapshot(ctx context.Context, chatID, json string) error {
	_, err := s.exec(ctx, `
		INSERT INTO github_snapshot (chat_id, json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
		chatID, json, time.Now().UTC())
	return err
}

// GetReviewBaseline returns the patch-id list quack last delivered a review at.
func (s *ghStore) GetReviewBaseline(ctx context.Context, chatID string) (string, bool, error) {
	return s.getChatString(ctx, `SELECT patch_ids FROM github_review_baseline WHERE chat_id = ?`, chatID)
}

// SetReviewBaseline upserts the patch-id list (only when a review is delivered).
func (s *ghStore) SetReviewBaseline(ctx context.Context, chatID, patchIDsJSON string) error {
	_, err := s.exec(ctx, `
		INSERT INTO github_review_baseline (chat_id, patch_ids, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET patch_ids = excluded.patch_ids, updated_at = excluded.updated_at`,
		chatID, patchIDsJSON, time.Now().UTC())
	return err
}

// DeleteChatHistory drops a chat's snapshot and review baseline once its issue/PR closes.
func (s *ghStore) DeleteChatHistory(ctx context.Context, chatID string) error {
	for _, q := range []string{
		`DELETE FROM github_snapshot WHERE chat_id = ?`,
		`DELETE FROM github_review_baseline WHERE chat_id = ?`,
	} {
		if _, err := s.exec(ctx, q, chatID); err != nil {
			return err
		}
	}
	return nil
}

// FixState tracks the CI auto-heal loop bound for one PR chat.
type FixState struct {
	ChatID  string
	LastSHA string
	Stopped bool
}

// GetFixState returns the auto-heal state, or (nil, nil) when none exists.
func (s *ghStore) GetFixState(ctx context.Context, chatID string) (*FixState, error) {
	var fs FixState
	err := s.queryRow(ctx, `SELECT chat_id, last_sha, stopped FROM github_fix_state WHERE chat_id = ?`, chatID).
		Scan(&fs.ChatID, &fs.LastSHA, &fs.Stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fs, nil
}

// SetFixState upserts the auto-heal state (persisted before the fix run so a crash doesn't refund it).
func (s *ghStore) SetFixState(ctx context.Context, fs FixState) error {
	_, err := s.exec(ctx, `
		INSERT INTO github_fix_state (chat_id, last_sha, stopped, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET last_sha = excluded.last_sha, stopped = excluded.stopped, updated_at = excluded.updated_at`,
		fs.ChatID, fs.LastSHA, fs.Stopped, time.Now().UTC())
	return err
}

// DeleteFixState re-arms auto-heal (human re-applied the fix label).
func (s *ghStore) DeleteFixState(ctx context.Context, chatID string) error {
	_, err := s.exec(ctx, `DELETE FROM github_fix_state WHERE chat_id = ?`, chatID)
	return err
}

// MergeIntent is a durable standing merge authorization for a PR chat. DispatchedHead is the head a
// push-triggered re-review last went out for, so two synchronize events for one head dispatch once.
type MergeIntent struct {
	ChatID         string
	RequestedBy    string
	DispatchedHead string
	CreatedAt      time.Time
}

// GetMergeIntent returns the merge authorization, or (nil, nil) when none.
func (s *ghStore) GetMergeIntent(ctx context.Context, chatID string) (*MergeIntent, error) {
	var mi MergeIntent
	err := s.queryRow(ctx, `SELECT chat_id, requested_by, dispatched_head, created_at FROM github_merge_intent WHERE chat_id = ?`, chatID).
		Scan(&mi.ChatID, &mi.RequestedBy, &mi.DispatchedHead, &mi.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mi, nil
}

// SetMergeIntent upserts the merge authorization. dispatched_head is kept on conflict: re-labeling must
// not forget a head a push-triggered re-review already covered.
func (s *ghStore) SetMergeIntent(ctx context.Context, chatID, requestedBy string) error {
	now := time.Now().UTC()
	_, err := s.exec(ctx, `
		INSERT INTO github_merge_intent (chat_id, requested_by, dispatched_head, created_at, updated_at) VALUES (?, ?, '', ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET requested_by = excluded.requested_by, updated_at = excluded.updated_at`,
		chatID, requestedBy, now, now)
	return err
}

// SetMergeIntentDispatchedHead records the head a push-triggered re-review went out for; a no-op if
// the intent was cleared underneath (unlabel/close racing the push).
func (s *ghStore) SetMergeIntentDispatchedHead(ctx context.Context, chatID, head string) error {
	_, err := s.exec(ctx, `UPDATE github_merge_intent SET dispatched_head = ?, updated_at = ? WHERE chat_id = ?`,
		head, time.Now().UTC(), chatID)
	return err
}

// DeleteMergeIntent clears the merge authorization (consumed by merge).
func (s *ghStore) DeleteMergeIntent(ctx context.Context, chatID string) error {
	_, err := s.exec(ctx, `DELETE FROM github_merge_intent WHERE chat_id = ?`, chatID)
	return err
}

// PendingRunRow is the durable subset of pendingRun: e.pending doesn't survive a restart, and without
// this row a run resumed at boot would drop its outcome (including a standing-intent merge).
type PendingRunRow struct {
	ChatID, SessionID, Owner, Repo, Login string
	Number                                int
	IsPR, IsPlan, IsLabelTrigger          bool
	CommentID, InstallationID             int64
	DefaultBranch, CloneURL               string
}

// SetPendingRun persists a dispatch's coordinates before Dispatch is called,
// so RunEnded can rebuild them if this process restarts before it fires.
func (s *ghStore) SetPendingRun(ctx context.Context, r PendingRunRow) error {
	_, err := s.exec(ctx, `
		INSERT INTO github_pending_run (chat_id, session_id, owner, repo, number, is_pr, login, is_plan, is_label_trigger, comment_id, default_branch, installation_id, clone_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET session_id = excluded.session_id, owner = excluded.owner, repo = excluded.repo,
			number = excluded.number, is_pr = excluded.is_pr, login = excluded.login, is_plan = excluded.is_plan,
			is_label_trigger = excluded.is_label_trigger, comment_id = excluded.comment_id,
			default_branch = excluded.default_branch, installation_id = excluded.installation_id,
			clone_url = excluded.clone_url, created_at = excluded.created_at`,
		r.ChatID, r.SessionID, r.Owner, r.Repo, r.Number, r.IsPR, r.Login, r.IsPlan,
		r.IsLabelTrigger, r.CommentID, r.DefaultBranch, r.InstallationID, r.CloneURL, time.Now().UTC())
	return err
}

// GetPendingRun returns the persisted dispatch coordinates, or (nil, nil) when none exist.
func (s *ghStore) GetPendingRun(ctx context.Context, chatID string) (*PendingRunRow, error) {
	var r PendingRunRow
	err := s.queryRow(ctx, `
		SELECT chat_id, session_id, owner, repo, number, is_pr, login, is_plan, is_label_trigger, comment_id, default_branch, installation_id, clone_url
		FROM github_pending_run WHERE chat_id = ?`, chatID).
		Scan(&r.ChatID, &r.SessionID, &r.Owner, &r.Repo, &r.Number, &r.IsPR, &r.Login, &r.IsPlan, &r.IsLabelTrigger,
			&r.CommentID, &r.DefaultBranch, &r.InstallationID, &r.CloneURL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeletePendingRun clears a dispatch's persisted coordinates (consumed by finalize).
func (s *ghStore) DeletePendingRun(ctx context.Context, chatID string) error {
	_, err := s.exec(ctx, `DELETE FROM github_pending_run WHERE chat_id = ?`, chatID)
	return err
}

// PendingClaims maps each pending run's session to its dispatch time: the inflight claim it held before a restart.
func (s *ghStore) PendingClaims(ctx context.Context) (map[string]time.Time, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT session_id, created_at FROM github_pending_run`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	claims := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		claims[id] = at
	}
	return claims, rows.Err()
}

// PrunePendingRuns deletes rows created before cutoff: runs that never reached RunEnded.
func (s *ghStore) PrunePendingRuns(ctx context.Context, cutoff time.Time) error {
	_, err := s.exec(ctx, `DELETE FROM github_pending_run WHERE created_at < ?`, cutoff.UTC())
	return err
}
