// Package ledger is quack's write-ahead log: one append-only, per-chat stream of typed Entry rows shared by
// intents and observations.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrStaleParent: another entry already claimed (chat_id, key, parent_revision). Nothing was written; the
// caller rereads the real latest and retries.
var ErrStaleParent = errors.New("ledger: parent revision already claimed")

// DuplicateIntentError: entry.IdempotencyKey already exists for this chat. Nothing was written; Existing is
// the winning entry, and callers treat this as a no-op.
type DuplicateIntentError struct{ Existing Entry }

func (e *DuplicateIntentError) Error() string {
	return fmt.Sprintf("ledger: duplicate idempotency key, existing seq %d", e.Existing.Seq)
}

// ParentRevisionOf reads an artifact.revision payload's parent_revision, so stores can enforce uniqueness
// without owning recordstore's payload type.
func ParentRevisionOf(kind string, payload json.RawMessage) int64 {
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

// LedgerStore is the WAL backend; only Postgres gives the transactional, gapless seq the intent path needs.
// Never mutate or delete a single entry; Delete only drops a chat.
type LedgerStore interface {
	// AppendIntent allocates entry.Seq and writes entry atomically. On error nothing was written and the
	// caller must not perform the state change the entry describes.
	AppendIntent(ctx context.Context, entry Entry) (seq int64, err error)
	// ReadEntries returns every Entry for chatID with Seq >= fromSeq, in seq order.
	ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error)
	// MaxSeq returns chatID's highest entry Seq, 0 if it has none - a cheap
	// alternative to ReadEntries for callers that only need "how far along is this chat".
	MaxSeq(ctx context.Context, chatID string) (int64, error)
	// List returns every chat with at least one entry.
	List(ctx context.Context) ([]SessionRef, error)
	// Delete removes a whole chat's entries (chat hard-delete only).
	Delete(ctx context.Context, sessionID string) error
}

// FilteredReader is a LedgerStore that can push a kind filter to the database instead of decoding every
// payload just to discard it.
type FilteredReader interface {
	ReadEntriesFiltered(ctx context.Context, chatID string, fromSeq int64, kinds []string) ([]Entry, error)
}

// CrossChatFilteredReader is a LedgerStore that can filter by kind and `at >= since` across every chat in
// one query.
type CrossChatFilteredReader interface {
	ReadEntriesFilteredSince(ctx context.Context, kinds []string, since time.Time) ([]Entry, error)
}

// ReadAllByKindsSince returns every chat's entries with Kind in kinds and At >= since, via the store's
// CrossChatFilteredReader when present, else a per-chat fallback with identical results.
func ReadAllByKindsSince(ctx context.Context, store LedgerStore, kinds []string, since time.Time) ([]Entry, error) {
	if cr, ok := store.(CrossChatFilteredReader); ok {
		return cr.ReadEntriesFilteredSince(ctx, kinds, since)
	}
	chats, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, c := range chats {
		entries, err := ReadByKinds(ctx, store, c.ID, 0, kinds)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.At.Before(since) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// ReadByKinds returns chatID's entries with Kind in kinds and Seq >= fromSeq, in seq order, pushing the
// filter to SQL when store is a FilteredReader.
func ReadByKinds(ctx context.Context, store LedgerStore, chatID string, fromSeq int64, kinds []string) ([]Entry, error) {
	if fr, ok := store.(FilteredReader); ok {
		return fr.ReadEntriesFiltered(ctx, chatID, fromSeq, kinds)
	}
	entries, err := store.ReadEntries(ctx, chatID, fromSeq)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if want[e.Kind] {
			out = append(out, e)
		}
	}
	return out, nil
}
