package ledger

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory LedgerStore for tests. It is not a runtime
// backend: nothing survives the process, and config refuses anything but
// Postgres as the WAL.
type MemStore struct {
	mu      sync.Mutex
	entries map[string][]Entry
}

var (
	_ LedgerStore = (*MemStore)(nil)
	_ LedgerStore = (*PGStore)(nil)
)

func NewMemStore() *MemStore { return &MemStore{entries: map[string][]Entry{}} }

// AppendIntent enforces the same two constraints PGStore does (#1144 P4) by
// scanning this chat's entries - fine for a test-only store.
func (s *MemStore) AppendIntent(_ context.Context, e Entry) (int64, error) {
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
				return 0, &DuplicateIntentError{Existing: ex}
			}
		}
	}
	if e.Kind == KindArtifactRevision {
		parent := parentRevisionOf(e.Kind, e.Payload)
		for _, ex := range s.entries[e.ChatID] {
			if ex.Kind == KindArtifactRevision && ex.Key == e.Key && parentRevisionOf(ex.Kind, ex.Payload) == parent {
				return 0, ErrStaleParent
			}
		}
	}
	e.Seq = int64(len(s.entries[e.ChatID])) + 1
	e.SchemaVersion = EntrySchemaVersion
	s.entries[e.ChatID] = append(s.entries[e.ChatID], e)
	return e.Seq, nil
}

func (s *MemStore) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries[chatID] {
		if e.Seq >= fromSeq {
			out = append(out, MigrateEntry(e))
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

func (s *MemStore) List(context.Context) ([]SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionRef, 0, len(s.entries))
	for id, es := range s.entries {
		if len(es) == 0 {
			continue
		}
		out = append(out, SessionRef{ID: id, Size: int64(len(es)), ModTime: es[len(es)-1].At})
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
