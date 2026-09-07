// Package ledger is quack's write-ahead log: one append-only, per-chat
// stream of typed Entry rows. Intents (artifact revisions, deliveries, node
// lifecycle) and observations (model/tool/agent calls, judge scores) share
// the same shape, the same seq space and the same reader API.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrStaleParent: another entry already claimed (chat_id, key,
// parent_revision) - the store-level index that replaced idLocks (#1144 P4).
// Nothing was written; the caller rereads the real latest and retries.
var ErrStaleParent = errors.New("ledger: parent revision already claimed")

// DuplicateIntentError: entry.IdempotencyKey already exists for this chat
// (#1144 P4) - nothing was written; Existing is the entry that won, and the
// caller treats this as a no-op rather than an error to surface.
type DuplicateIntentError struct{ Existing Entry }

func (e *DuplicateIntentError) Error() string {
	return fmt.Sprintf("ledger: duplicate idempotency key, existing seq %d", e.Existing.Seq)
}

// parentRevisionOf reads an artifact.revision entry's parent_revision out of
// its payload, so PGStore/MemStore enforce uniqueness without either owning
// recordstore's artifactRevisionPayload type.
func parentRevisionOf(kind string, payload json.RawMessage) int64 {
	if kind != KindArtifactRevision {
		return 0
	}
	var p struct {
		ParentRevision int64 `json:"parent_revision"`
	}
	_ = json.Unmarshal(payload, &p) // best-effort; unparseable payload just claims parent 0
	return p.ParentRevision
}

// SessionRef describes one recorded chat for a List call.
type SessionRef struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// LedgerStore is the WAL backend. Postgres is the only backend that gives
// the transactional, gapless seq the intent path needs; MemStore exists for
// tests. Never mutate or delete a single entry; Delete only drops a chat.
type LedgerStore interface {
	// AppendIntent allocates entry.Seq and writes entry atomically. A non-nil
	// error means nothing was written and the caller must not perform the
	// state change the entry describes.
	AppendIntent(ctx context.Context, entry Entry) (seq int64, err error)
	// ReadEntries returns every Entry for chatID with Seq >= fromSeq, in seq order.
	ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error)
	// MaxSeq returns chatID's highest entry Seq, 0 if it has none - a cheap
	// alternative to ReadEntries for callers that only need "how far along is this chat".
	MaxSeq(ctx context.Context, chatID string) (int64, error)
	// LastCheckpoint returns chatID's most recently appended KindCheckpoint
	// entry, false if it has none (#1144 P5). "Most recent" is a heuristic
	// for picking a fold starting point, not a correctness requirement - see
	// fold.Apply, which trusts the checkpoint payload's own LastSeq rather
	// than this entry's Seq.
	LastCheckpoint(ctx context.Context, chatID string) (Entry, bool, error)
	// List returns every chat with at least one entry.
	List(ctx context.Context) ([]SessionRef, error)
	// Delete removes a whole chat's entries (chat hard-delete only).
	Delete(ctx context.Context, sessionID string) error
}
