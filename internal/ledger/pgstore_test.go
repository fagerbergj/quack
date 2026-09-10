package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGDB returns a real Postgres container's raw connection, UNMIGRATED -
// callers migrating onto a pre-existing table need the db before NewPGStore's
// AutoMigrate runs. Skips (not fails) when Docker isn't reachable.
func newTestPGDB(t *testing.T) *gorm.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_ledger_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres ledger integration test: %v", err)
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
	return db
}

// newTestPGStore starts a real Postgres container - AppendIntent's seq
// allocation relies on a real UPSERT's row locking, which sqlite/mocks
// can't exercise. Skips (not fails) when Docker isn't reachable.
func newTestPGStore(t *testing.T) *PGStore {
	t.Helper()
	store, err := NewPGStore(newTestPGDB(t))
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
}

// TestPGStoreNewMigrate_AddsChatKeyIndexToExistingTable (#1111 review finding): every other
// PG test starts from a fresh container, so the production path - adding idx_ledger_chat_key
// to an EXISTING ledger_entries table - was otherwise unexercised; a tag-parsing miss would silently degrade ReadEntriesByKey to a scan with no test to catch it.
func TestPGStoreNewMigrate_AddsChatKeyIndexToExistingTable(t *testing.T) {
	t.Parallel()
	db := newTestPGDB(t)
	if err := db.Exec(`CREATE TABLE ledger_entries (
		id BIGSERIAL PRIMARY KEY, chat_id TEXT, seq BIGINT, turn_id TEXT,
		node_id TEXT, kind TEXT, key TEXT, at TIMESTAMPTZ, payload JSONB)`).Error; err != nil {
		t.Fatalf("create bare table: %v", err)
	}
	if _, err := NewPGStore(db); err != nil { // AutoMigrate onto the pre-existing table
		t.Fatalf("NewPGStore: %v", err)
	}
	var n int
	if err := db.Raw(`SELECT count(*) FROM pg_indexes
		WHERE tablename = 'ledger_entries' AND indexname = 'idx_ledger_chat_key'`).Scan(&n).Error; err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if n != 1 {
		t.Fatalf("idx_ledger_chat_key = %d, want 1 on a pre-existing table", n)
	}
}

// TestPGStoreAppendIntentConcurrentSeqIsGaplessAndUnique is verification
// case 12 (V4 §7): N goroutines racing AppendIntent on one chat must land
// on seq exactly 1..N, no gaps, no duplicates, no error. Run with -race.
func TestPGStoreAppendIntentConcurrentSeqIsGaplessAndUnique(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const n = 50
	const chatID = "chat-concurrent"

	var wg sync.WaitGroup
	seqs := make([]int64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := store.AppendIntent(ctx, Entry{
				ChatID:  chatID,
				Kind:    KindNodeStarted,
				Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			})
			seqs[i] = seq
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("AppendIntent[%d] failed: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("duplicate seq %d", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for want := int64(1); want <= n; want++ {
		if !seen[want] {
			t.Fatalf("seq %d missing: got %v", want, seqs)
		}
	}
}

// TestPGStoreReadEntriesReturnsInOrder is verification case (c).
func TestPGStoreReadEntriesReturnsInOrder(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID = "chat-order"

	var lastSeq int64
	for i := 0; i < 5; i++ {
		// Distinct parent_revision per entry: they share a key, and the
		// #1144 P4 unique (chat_id, key, parent_revision) index would reject
		// a repeat.
		payload, err := json.Marshal(struct {
			ParentRevision int `json:"parent_revision"`
		}{ParentRevision: i})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		seq, err := store.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindArtifactRevision, Key: "code_review:pr:1", Payload: payload})
		if err != nil {
			t.Fatalf("AppendIntent %d: %v", i, err)
		}
		lastSeq = seq
	}

	entries, err := store.ReadEntries(ctx, chatID, 1)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}

	fromMiddle, err := store.ReadEntries(ctx, chatID, lastSeq)
	if err != nil {
		t.Fatalf("ReadEntries fromSeq=%d: %v", lastSeq, err)
	}
	if len(fromMiddle) != 1 || fromMiddle[0].Seq != lastSeq {
		t.Errorf("ReadEntries fromSeq=%d = %+v, want exactly seq %d", lastSeq, fromMiddle, lastSeq)
	}
}

// TestPGStoreReadEntriesFiltered is perf audit #1: a kinds filter must
// return exactly the matching rows, in seq order, same as ReadEntries then
// filtering in Go - and ReadByKinds must push it to SQL for a FilteredReader.
func TestPGStoreReadEntriesFiltered(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID = "chat-filtered"

	kinds := []string{KindNodeStarted, KindNodeDone, KindMemoryVote}
	for i, kind := range []string{KindNodeStarted, KindLLMCall, KindNodeDone, KindAgentInvoke, KindMemoryVote} {
		if _, err := store.AppendIntent(ctx, Entry{ChatID: chatID, Kind: kind, Key: fmt.Sprintf("k%d", i), Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("AppendIntent %d: %v", i, err)
		}
	}

	var _ FilteredReader = store // pgstore must satisfy the optional interface ReadByKinds looks for
	entries, err := ReadByKinds(ctx, store, chatID, 0, kinds)
	if err != nil {
		t.Fatalf("ReadByKinds: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	want := map[string]bool{KindNodeStarted: true, KindNodeDone: true, KindMemoryVote: true}
	for i, e := range entries {
		if !want[e.Kind] {
			t.Errorf("entries[%d].Kind = %q, not in filter", i, e.Kind)
		}
		if i > 0 && entries[i-1].Seq >= e.Seq {
			t.Errorf("entries not in seq order: %+v", entries)
		}
	}
}

// TestPGStoreReadEntriesFilteredSince pins perf audit #12's fix: one cross-chat
// `kind IN (?) AND at >= ?` query must return exactly what the old List() + per-chat
// ReadByKinds + Go-side time filter did - kind-filtered, window-filtered, across every chat.
func TestPGStoreReadEntriesFilteredSince(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kinds := []string{KindMemoryVote, KindMemoryRecall}

	type seed struct {
		chatID, kind string
		at           time.Time
	}
	seeds := []seed{
		{"chat-a", KindMemoryVote, now.Add(-1 * time.Hour)},       // in window, wanted kind
		{"chat-a", KindMemoryRecall, now.Add(-2 * time.Hour)},     // in window, wanted kind
		{"chat-a", KindLLMCall, now.Add(-1 * time.Hour)},          // in window, unwanted kind
		{"chat-b", KindMemoryVote, now.Add(-30 * 24 * time.Hour)}, // outside the window
	}
	for i, sd := range seeds {
		if _, err := store.AppendIntent(ctx, Entry{
			ChatID: sd.chatID, Kind: sd.kind, Key: fmt.Sprintf("k%d", i), At: sd.at, Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("AppendIntent %d: %v", i, err)
		}
	}

	var _ CrossChatFilteredReader = store // pgstore must satisfy the optional interface ReadAllByKindsSince looks for
	since := now.Add(-7 * 24 * time.Hour)
	got, err := ReadAllByKindsSince(ctx, store, kinds, since)
	if err != nil {
		t.Fatalf("ReadAllByKindsSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.ChatID != "chat-a" {
			t.Errorf("entry from chat %q, want only chat-a (chat-b is outside the window)", e.ChatID)
		}
		if e.Kind != KindMemoryVote && e.Kind != KindMemoryRecall {
			t.Errorf("entry kind = %q, not in filter", e.Kind)
		}
	}
}

// TestPGStoreAppendIntentValidation is verification case 11's other half:
// a malformed intent (no chat_id/kind) is rejected without writing anything.
func TestPGStoreAppendIntentValidation(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()

	if _, err := store.AppendIntent(ctx, Entry{Kind: KindNodeStarted}); err == nil {
		t.Error("expected error for missing chat_id")
	}
	if _, err := store.AppendIntent(ctx, Entry{ChatID: "c"}); err == nil {
		t.Error("expected error for missing kind")
	}
}

// artifactRevPayload builds an artifact.revision entry's payload with just
// the parent_revision field the unique index cares about - recordstore's
// real payload carries more, but PGStore only ever reads this one field.
func artifactRevPayload(t *testing.T, parent int) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(struct {
		ParentRevision int `json:"parent_revision"`
	}{ParentRevision: parent})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestPGStoreAppendIntent_ParentRevisionConflict (#1144 P4, real Postgres): two
// goroutines saving the same artifact id, both claiming the same parent_revision -
// exactly one wins, the other gets ErrStaleParent, neither overwrites the other's WAL entry. Run with -race.
func TestPGStoreAppendIntent_ParentRevisionConflict(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID, key = "chat-conflict", "code_review:pr-1"

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.AppendIntent(ctx, Entry{
				ChatID: chatID, Kind: KindArtifactRevision, Key: key,
				Payload: artifactRevPayload(t, 0), // every goroutine claims the SAME parent
			})
		}(i)
	}
	wg.Wait()

	var wins, conflicts int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrStaleParent):
			conflicts++
		default:
			t.Fatalf("AppendIntent[%d]: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly 1 winner and %d ErrStaleParent", wins, conflicts, n-1)
	}

	entries, err := store.ReadEntries(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d rows for %s, want exactly 1 (the one winner, no silent duplicate)", len(entries), key)
	}
}

// TestPGStoreAppendIntent_IdempotencyKeyIsANoOp is #1144 P4's other store-level
// guarantee: a repeated IdempotencyKey writes nothing and hands back the
// entry that already claimed it, instead of erroring or duplicating.
func TestPGStoreAppendIntent_IdempotencyKeyIsANoOp(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID = "chat-idem"

	seq1, err := store.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindArtifactRevision, Key: "id1", IdempotencyKey: "dup"})
	if err != nil {
		t.Fatalf("first AppendIntent: %v", err)
	}
	_, err = store.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindArtifactRevision, Key: "id1", Payload: artifactRevPayload(t, 1), IdempotencyKey: "dup"})
	var dup *DuplicateIntentError
	if !errors.As(err, &dup) {
		t.Fatalf("second AppendIntent = %v, want *DuplicateIntentError", err)
	}
	if dup.Existing.Seq != seq1 {
		t.Fatalf("DuplicateIntentError.Existing.Seq = %d, want the first entry's %d", dup.Existing.Seq, seq1)
	}
	entries, err := store.ReadEntries(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d rows, want exactly 1 (the duplicate wrote nothing)", len(entries))
	}
}

// TestPGStoreNewMigrate_RefusesDuplicateParentRevisions: an existing ledger_entries table
// already violating (chat_id, key, parent_revision) uniqueness (only reachable pre-#1144
// P4, since idLocks made it essentially impossible) must not get the new index silently skipped or created broken - NewPGStore refuses to start instead.
func TestPGStoreNewMigrate_RefusesDuplicateParentRevisions(t *testing.T) {
	t.Parallel()
	db := newTestPGDB(t)
	if err := db.Exec(`CREATE TABLE ledger_entries (
		id BIGSERIAL PRIMARY KEY, chat_id TEXT, seq BIGINT, turn_id TEXT,
		node_id TEXT, agent TEXT, round TEXT, kind TEXT, key TEXT, at TIMESTAMPTZ,
		payload JSONB, parent_revision BIGINT, idempotency_key TEXT)`).Error; err != nil {
		t.Fatalf("create bare table: %v", err)
	}
	// Two rows already claiming the same (chat_id, key, parent_revision) -
	// simulates a database restored from before this index existed.
	if err := db.Exec(`INSERT INTO ledger_entries (chat_id, seq, kind, key, at, payload, parent_revision)
		VALUES ('chat1', 1, 'artifact.revision', 'id1', now(), '{}', 0), ('chat1', 2, 'artifact.revision', 'id1', now(), '{}', 0)`).Error; err != nil {
		t.Fatalf("seed duplicates: %v", err)
	}
	if _, err := NewPGStore(db); err == nil {
		t.Fatal("NewPGStore succeeded despite pre-existing duplicate parent revisions")
	}
}

// TestPGStoreNewMigrate_BackfillsNullParentRevisionBeforeDedup: ADD COLUMN leaves
// parent_revision NULL on pre-existing rows, and two different revisions both NULL
// would look like a duplicate (GROUP BY folds NULLs) and wedge the deploy - backfill from the payload first, so boot succeeds.
func TestPGStoreNewMigrate_BackfillsNullParentRevisionBeforeDedup(t *testing.T) {
	t.Parallel()
	db := newTestPGDB(t)
	if err := db.Exec(`CREATE TABLE ledger_entries (
		id BIGSERIAL PRIMARY KEY, chat_id TEXT, seq BIGINT, turn_id TEXT,
		node_id TEXT, agent TEXT, round TEXT, kind TEXT, key TEXT, at TIMESTAMPTZ,
		payload JSONB, parent_revision BIGINT, idempotency_key TEXT)`).Error; err != nil {
		t.Fatalf("create bare table: %v", err)
	}
	if err := db.Exec(`INSERT INTO ledger_entries (chat_id, seq, kind, key, at, payload) VALUES
		('chat1', 1, 'artifact.revision', 'id1', now(), '{"revision":1,"parent_revision":0}'),
		('chat1', 2, 'artifact.revision', 'id1', now(), '{"revision":2,"parent_revision":1}')`).Error; err != nil {
		t.Fatalf("seed pre-existing NULL-column rows: %v", err)
	}
	if _, err := NewPGStore(db); err != nil {
		t.Fatalf("NewPGStore refused to start on legitimate pre-existing rows with a NULL parent_revision column: %v", err)
	}
	var got []int64
	if err := db.Raw(`SELECT parent_revision FROM ledger_entries WHERE chat_id = 'chat1' ORDER BY seq`).Scan(&got).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("parent_revision after backfill = %v, want [0 1]", got)
	}
}

// TestRecoverInvalidConcurrentIndex_DropsAndAllowsRebuild: a unique index
// built concurrently over duplicate data reproduces Postgres' real invalid-index shape without synthesizing one.
func TestRecoverInvalidConcurrentIndex_DropsAndAllowsRebuild(t *testing.T) {
	t.Parallel()
	db := newTestPGDB(t)
	if err := db.Exec(`CREATE TABLE t1350 (id int, kind text)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec(`INSERT INTO t1350 (id, kind) VALUES (1, 'a'), (1, 'b')`).Error; err != nil {
		t.Fatalf("seed duplicate data: %v", err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX CONCURRENTLY idx_t1350 ON t1350 (id)`).Error; err == nil {
		t.Fatal("expected the duplicate-data unique index build to fail")
	}
	var invalid int64
	if err := db.Raw(`SELECT count(*) FROM pg_index WHERE indexrelid = 'idx_t1350'::regclass AND NOT indisvalid`).
		Scan(&invalid).Error; err != nil || invalid != 1 {
		t.Fatalf("setup: failed build did not leave an invalid index (invalid=%d, err=%v)", invalid, err)
	}

	if err := recoverInvalidConcurrentIndex(db, "idx_t1350"); err != nil {
		t.Fatalf("recoverInvalidConcurrentIndex: %v", err)
	}
	var exists int64
	if err := db.Raw(`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_t1350'`).Scan(&exists).Error; err != nil {
		t.Fatalf("check dropped: %v", err)
	}
	if exists != 0 {
		t.Fatal("invalid index still present after recovery")
	}
	// A rebuild over the same (now non-unique) index must succeed - proves
	// the boot path is unblocked, not just that the row disappeared.
	if err := db.Exec(`CREATE INDEX CONCURRENTLY idx_t1350 ON t1350 (id)`).Error; err != nil {
		t.Fatalf("rebuild after recovery: %v", err)
	}
}

// TestRecoverInvalidConcurrentIndex_NoOpWhenValidOrAbsent: the boot path
// calls this on every start, so it must never disturb a healthy index or
// error out when the index doesn't exist yet (first boot).
func TestRecoverInvalidConcurrentIndex_NoOpWhenValidOrAbsent(t *testing.T) {
	t.Parallel()
	db := newTestPGDB(t)
	if err := recoverInvalidConcurrentIndex(db, "idx_does_not_exist_yet"); err != nil {
		t.Fatalf("absent index: %v", err)
	}
	if err := db.Exec(`CREATE TABLE t1350b (id int, kind text)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec(`CREATE INDEX idx_t1350b ON t1350b (id)`).Error; err != nil {
		t.Fatalf("create valid index: %v", err)
	}
	if err := recoverInvalidConcurrentIndex(db, "idx_t1350b"); err != nil {
		t.Fatalf("valid index: %v", err)
	}
	var exists int64
	if err := db.Raw(`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_t1350b'`).Scan(&exists).Error; err != nil || exists != 1 {
		t.Fatalf("valid index was disturbed: exists=%d, err=%v", exists, err)
	}
}
