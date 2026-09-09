package ledger_test

import (
	"context"
	"encoding/json"
	"testing"

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
