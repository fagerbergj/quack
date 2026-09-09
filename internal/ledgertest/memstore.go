// Package ledgertest provides an in-memory ledger.LedgerStore for tests.
// It is not a runtime backend: nothing survives the process, and config
// refuses anything but Postgres as the WAL.
package ledgertest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// MemStore is the in-memory ledger.LedgerStore for tests.
type MemStore struct {
	mu      sync.Mutex
	entries map[string][]ledger.Entry
}

var (
	_ ledger.LedgerStore             = (*MemStore)(nil)
	_ ledger.CrossChatFilteredReader = (*MemStore)(nil)
)

func NewMemStore() *MemStore { return &MemStore{entries: map[string][]ledger.Entry{}} }

// AppendIntent enforces the same two constraints PGStore does by
// scanning this chat's entries - fine for a test-only store.
func (s *MemStore) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	if e.ChatID == "" || e.Kind == "" {
		return 0, fmt.Errorf("ledger: intent needs chat_id and kind")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotency before parent-conflict: a repeat's stale parent must not
	// mask its own no-op, or a caller retrying on ErrStaleParent loops forever.
	if e.IdempotencyKey != "" {
		for _, ex := range s.entries[e.ChatID] {
			if ex.IdempotencyKey == e.IdempotencyKey {
				return 0, &ledger.DuplicateIntentError{Existing: ex}
			}
		}
	}
	if e.Kind == ledger.KindArtifactRevision {
		parent := ledger.ParentRevisionOf(e.Kind, e.Payload)
		for _, ex := range s.entries[e.ChatID] {
			if ex.Kind == ledger.KindArtifactRevision && ex.Key == e.Key && ledger.ParentRevisionOf(ex.Kind, ex.Payload) == parent {
				return 0, ledger.ErrStaleParent
			}
		}
	}
	e.Seq = int64(len(s.entries[e.ChatID])) + 1
	e.SchemaVersion = ledger.EntrySchemaVersion
	s.entries[e.ChatID] = append(s.entries[e.ChatID], e)
	return e.Seq, nil
}

func (s *MemStore) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ledger.Entry
	for _, e := range s.entries[chatID] {
		if e.Seq >= fromSeq {
			out = append(out, ledger.MigrateEntry(e))
		}
	}
	return out, nil
}

// ReadEntriesFilteredSince implements ledger.CrossChatFilteredReader, so a test using
// MemStore exercises the same one-query code path PGStore does (perf audit #12) rather than
// always falling back to ReadAllByKindsSince's per-chat loop.
func (s *MemStore) ReadEntriesFilteredSince(_ context.Context, kinds []string, since time.Time) ([]ledger.Entry, error) {
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []ledger.Entry
	for _, id := range ids {
		for _, e := range s.entries[id] {
			if want[e.Kind] && !e.At.Before(since) {
				out = append(out, ledger.MigrateEntry(e))
			}
		}
	}
	return out, nil
}

// MaxSeq mirrors PGStore's: entries are appended with Seq = position+1, so
// the count IS the max seq (0 for a chat with none).
func (s *MemStore) MaxSeq(_ context.Context, chatID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.entries[chatID])), nil
}

func (s *MemStore) List(context.Context) ([]ledger.SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ledger.SessionRef, 0, len(s.entries))
	for id, es := range s.entries {
		if len(es) == 0 {
			continue
		}
		out = append(out, ledger.SessionRef{ID: id, Size: int64(len(es)), ModTime: es[len(es)-1].At})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemStore) Delete(_ context.Context, chatID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, chatID)
	return nil
}
