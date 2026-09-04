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

func (s *MemStore) AppendIntent(_ context.Context, e Entry) (int64, error) {
	if e.ChatID == "" || e.Kind == "" {
		return 0, fmt.Errorf("ledger: intent needs chat_id and kind")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = int64(len(s.entries[e.ChatID])) + 1
	s.entries[e.ChatID] = append(s.entries[e.ChatID], e)
	return e.Seq, nil
}

func (s *MemStore) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries[chatID] {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
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
