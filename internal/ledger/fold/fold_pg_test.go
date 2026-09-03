package fold

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGStore mirrors internal/ledger's own container test helper (kept
// package-local here since fold needs the REAL PGStore, not FSStore, to
// exercise ReadEntriesByKey/ReadEntriesPage and the (chat_id, key) index).
// Skips (not fails) when Docker isn't reachable.
func newTestPGStore(t *testing.T) *ledger.PGStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_ledger_fold_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres fold integration test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	store, err := ledger.NewPGStore(db)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
}

// TestFold_Postgres_KeyIndexAndPaging exercises the real PGStore path:
// ReadEntriesByKey (the (chat_id, key) index) and ReadEntriesPage, proving
// LastRevision and Fold agree with each other against a real database, not
// just FSStore's in-memory scan.
func TestFold_Postgres_KeyIndexAndPaging(t *testing.T) {
	// No t.Parallel(): this test mutates the package-level pageSize var, as
	// does TestFold_PagingMatchesOneSlice - running both non-parallel is what
	// keeps that shared mutation race-free (#1111 review finding). Add
	// t.Parallel() back only after threading the page limit into readAll
	// instead of sharing pageSize.
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

	rev, err := LastRevision(ctx, store, chatID, "id1")
	if err != nil {
		t.Fatalf("LastRevision: %v", err)
	}
	if rev != 12 {
		t.Fatalf("LastRevision = %d, want 12", rev)
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
