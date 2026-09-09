package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

// TestReadByKindsFallbackFilters is perf audit #1's fallback path: a store
// with no FilteredReader (MemStore, and every test fake) must still return
// only the requested kinds, in seq order - ReadByKinds does the filtering
// itself instead of pushing it to SQL.
func TestReadByKindsFallbackFilters(t *testing.T) {
	ctx := context.Background()
	store := ledgertest.NewMemStore()
	const chatID = "chat-1"
	kinds := []string{ledger.KindNodeStarted, ledger.KindMemoryVote, ledger.KindLLMCall, ledger.KindNodeDone, ledger.KindMemoryVote}
	for _, k := range kinds {
		if _, err := store.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: k, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("AppendIntent %q: %v", k, err)
		}
	}

	got, err := ledger.ReadByKinds(ctx, store, chatID, 0, []string{ledger.KindNodeStarted, ledger.KindNodeDone})
	if err != nil {
		t.Fatalf("ReadByKinds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Kind != ledger.KindNodeStarted || got[1].Kind != ledger.KindNodeDone {
		t.Errorf("got kinds %q, %q, want node.started, node.done in seq order", got[0].Kind, got[1].Kind)
	}
}

// TestReadByKindsEmptyResult confirms a filter matching nothing returns an
// empty (not nil-panicking, not error) slice.
func TestReadByKindsEmptyResult(t *testing.T) {
	ctx := context.Background()
	store := ledgertest.NewMemStore()
	if _, err := store.AppendIntent(ctx, ledger.Entry{ChatID: "chat-1", Kind: ledger.KindLLMCall, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("AppendIntent: %v", err)
	}
	got, err := ledger.ReadByKinds(ctx, store, "chat-1", 0, []string{ledger.KindMemoryVote})
	if err != nil {
		t.Fatalf("ReadByKinds: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0: %+v", len(got), got)
	}
}

// noPushdownStore forwards to MemStore WITHOUT embedding it, so
// ReadEntriesFilteredSince is not promoted and ReadAllByKindsSince must take its
// List()-plus-per-chat fallback path - the same shape MemStore/every test fake in the repo
// already used before perf audit #12's fix.
type noPushdownStore struct{ mem *ledgertest.MemStore }

func (s noPushdownStore) AppendIntent(ctx context.Context, e ledger.Entry) (int64, error) {
	return s.mem.AppendIntent(ctx, e)
}
func (s noPushdownStore) ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	return s.mem.ReadEntries(ctx, chatID, fromSeq)
}
func (s noPushdownStore) MaxSeq(ctx context.Context, chatID string) (int64, error) {
	return s.mem.MaxSeq(ctx, chatID)
}
func (s noPushdownStore) List(ctx context.Context) ([]ledger.SessionRef, error) {
	return s.mem.List(ctx)
}
func (s noPushdownStore) Delete(ctx context.Context, chatID string) error {
	return s.mem.Delete(ctx, chatID)
}

var _ ledger.LedgerStore = noPushdownStore{}

// TestReadAllByKindsSinceFallbackMatchesPushdown pins perf audit #12: the fallback path
// (List + per-chat ReadByKinds + Go-side time filter) and the pushdown path
// (ReadEntriesFilteredSince) must return the identical set of entries for the same seed.
func TestReadAllByKindsSinceFallbackMatchesPushdown(t *testing.T) {
	ctx := context.Background()
	mem := ledgertest.NewMemStore()
	now := time.Now().UTC()
	kinds := []string{ledger.KindMemoryVote, ledger.KindMemoryRecall}

	type seed struct {
		chatID, kind string
		at           time.Time
	}
	seeds := []seed{
		{"chat-a", ledger.KindMemoryVote, now.Add(-1 * time.Hour)},
		{"chat-a", ledger.KindMemoryRecall, now.Add(-2 * time.Hour)},
		{"chat-a", ledger.KindLLMCall, now.Add(-1 * time.Hour)},
		{"chat-b", ledger.KindMemoryVote, now.Add(-30 * 24 * time.Hour)},
		{"chat-b", ledger.KindMemoryRecall, now.Add(-1 * time.Hour)},
	}
	for i, sd := range seeds {
		if _, err := mem.AppendIntent(ctx, ledger.Entry{
			ChatID: sd.chatID, Kind: sd.kind, Key: fmt.Sprintf("k%d", i), At: sd.at, Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("AppendIntent %d: %v", i, err)
		}
	}

	since := now.Add(-7 * 24 * time.Hour)
	pushdown, err := ledger.ReadAllByKindsSince(ctx, mem, kinds, since)
	if err != nil {
		t.Fatalf("ReadAllByKindsSince (pushdown): %v", err)
	}
	fallback, err := ledger.ReadAllByKindsSince(ctx, noPushdownStore{mem}, kinds, since)
	if err != nil {
		t.Fatalf("ReadAllByKindsSince (fallback): %v", err)
	}

	key := func(es []ledger.Entry) map[string]int {
		m := make(map[string]int, len(es))
		for _, e := range es {
			m[fmt.Sprintf("%s/%s/%d", e.ChatID, e.Kind, e.Seq)]++
		}
		return m
	}
	pd, fb := key(pushdown), key(fallback)
	if len(pushdown) != 3 || len(pushdown) != len(fallback) {
		t.Fatalf("pushdown=%d fallback=%d entries, want 3 and equal: pushdown=%+v fallback=%+v", len(pushdown), len(fallback), pushdown, fallback)
	}
	for k, n := range pd {
		if fb[k] != n {
			t.Errorf("entry %q: pushdown=%d fallback=%d, want equal", k, n, fb[k])
		}
	}
}
