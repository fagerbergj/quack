package fold

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/pgtest"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGStore: fold needs the real PGStore to exercise ReadEntriesPage paging. Skips without Docker.
func newTestPGStore(t *testing.T) *ledger.PGStore {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgtest.DSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	store, err := ledger.NewPGStore(db)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
}

// TestFold_Postgres_KeyIndexAndPaging proves paged Fold against a real database.
func TestFold_Postgres_KeyIndexAndPaging(t *testing.T) {
	// No t.Parallel(): this and TestFold_PagingMatchesOneSlice both mutate the package-level pageSize.
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID = "chat-fold-pg"

	for i := 1; i <= 12; i++ {
		payload, err := json.Marshal(struct {
			Revision       int `json:"revision"`
			ParentRevision int `json:"parent_revision"`
		}{Revision: i, ParentRevision: i - 1})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := store.AppendIntent(ctx, ledger.Entry{
			ChatID: chatID, Kind: ledger.KindArtifactRevision, Key: "id1", Payload: payload,
		}); err != nil {
			t.Fatalf("AppendIntent: %v", err)
		}
	}
	// A second id interleaved: proves ReadEntriesByKey's filter (and the
	// index behind it) isolate "id1" from other keys in the same chat.
	other, _ := json.Marshal(struct {
		Revision       int `json:"revision"`
		ParentRevision int `json:"parent_revision"`
	}{Revision: 1, ParentRevision: 0})
	if _, err := store.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindArtifactRevision, Key: "id2", Payload: other}); err != nil {
		t.Fatalf("AppendIntent id2: %v", err)
	}

	old := pageSize
	pageSize = 3
	defer func() { pageSize = old }()
	res, err := Fold(ctx, store, chatID, 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if latest, ok := res.Artifacts["id1"].Latest(); !ok || latest.Revision != 12 {
		t.Fatalf("Fold latest id1 = %+v, ok=%v; want 12", latest, ok)
	}
	if len(res.Artifacts["id1"].Revisions) != 12 {
		t.Fatalf("Fold id1 revisions = %d, want 12", len(res.Artifacts["id1"].Revisions))
	}
	if _, ok := res.Artifacts["id2"]; !ok {
		t.Fatalf("Fold dropped id2 entirely")
	}
}
