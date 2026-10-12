package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// openStore is newStore plus an eager open, for tests that want migration errors up front.
func openStore(dataDir string) (*ghStore, error) {
	s := newStore(dataDir)
	if _, err := s.conn(); err != nil {
		return nil, err
	}
	return s, nil
}

func newTestStore(t *testing.T) *ghStore {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, ok, err := s.GetSnapshot(ctx, "c1"); err != nil || ok {
		t.Fatalf("GetSnapshot on empty store: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := s.SetSnapshot(ctx, "c1", `{"state":"open"}`); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	if j, ok, err := s.GetSnapshot(ctx, "c1"); err != nil || !ok || j != `{"state":"open"}` {
		t.Fatalf("GetSnapshot = (%q, %v, %v), want the stored JSON", j, ok, err)
	}

	if err := s.SetReviewBaseline(ctx, "c1", `["a","b"]`); err != nil {
		t.Fatalf("SetReviewBaseline: %v", err)
	}
	if p, ok, err := s.GetReviewBaseline(ctx, "c1"); err != nil || !ok || p != `["a","b"]` {
		t.Fatalf("GetReviewBaseline = (%q, %v, %v)", p, ok, err)
	}

	if err := s.SetFixState(ctx, FixState{ChatID: "c1", LastSHA: "abc123", Stopped: false}); err != nil {
		t.Fatalf("SetFixState: %v", err)
	}
	fs, err := s.GetFixState(ctx, "c1")
	if err != nil || fs == nil || fs.LastSHA != "abc123" || fs.Stopped {
		t.Fatalf("GetFixState = %+v, err=%v", fs, err)
	}
	if err := s.DeleteFixState(ctx, "c1"); err != nil {
		t.Fatalf("DeleteFixState: %v", err)
	}
	if fs, err := s.GetFixState(ctx, "c1"); err != nil || fs != nil {
		t.Fatalf("GetFixState after delete = %+v, err=%v, want nil", fs, err)
	}

	if err := s.SetMergeIntent(ctx, "c1", "alice"); err != nil {
		t.Fatalf("SetMergeIntent: %v", err)
	}
	mi, err := s.GetMergeIntent(ctx, "c1")
	if err != nil || mi == nil || mi.RequestedBy != "alice" {
		t.Fatalf("GetMergeIntent = %+v, err=%v", mi, err)
	}
	if err := s.DeleteMergeIntent(ctx, "c1"); err != nil {
		t.Fatalf("DeleteMergeIntent: %v", err)
	}
	if mi, err := s.GetMergeIntent(ctx, "c1"); err != nil || mi != nil {
		t.Fatalf("GetMergeIntent after delete = %+v, err=%v, want nil", mi, err)
	}
}

// openStore adds dispatched_head in place to an older database instead of erroring with "duplicate column"
// on every later open, and the existing row survives with an empty dispatched_head.
func TestMigrationAddsDispatchedHeadColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE github_merge_intent (
		chat_id TEXT PRIMARY KEY,
		requested_by TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO github_merge_intent (chat_id, requested_by, created_at, updated_at) VALUES ('c1','alice','2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore on a pre-migration db: %v", err)
	}
	defer s.Close()
	mi, err := s.GetMergeIntent(context.Background(), "c1")
	if err != nil || mi == nil || mi.RequestedBy != "alice" || mi.DispatchedHead != "" {
		t.Fatalf("GetMergeIntent after migration = %+v, err=%v; want the pre-existing row preserved with an empty dispatched_head", mi, err)
	}
	if err := s.SetMergeIntentDispatchedHead(context.Background(), "c1", "abc123"); err != nil {
		t.Fatalf("SetMergeIntentDispatchedHead: %v", err)
	}
	if mi, err := s.GetMergeIntent(context.Background(), "c1"); err != nil || mi.DispatchedHead != "abc123" {
		t.Fatalf("GetMergeIntent = %+v, err=%v; want dispatched_head=abc123", mi, err)
	}

	// Re-opening an already-migrated database must not error on the "add
	// column" step running again.
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("re-opening an already-migrated db: %v", err)
	}
	s2.Close()
}

// The ON CONFLICT clause omits dispatched_head on purpose: re-labeling must not forget an already-dispatched
// head, or the next synchronize for that head would dispatch again.
func TestSetMergeIntentConflictLeavesDispatchedHeadUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SetMergeIntent(ctx, "c1", "alice"); err != nil {
		t.Fatalf("SetMergeIntent: %v", err)
	}
	if err := s.SetMergeIntentDispatchedHead(ctx, "c1", "abc123"); err != nil {
		t.Fatalf("SetMergeIntentDispatchedHead: %v", err)
	}
	if err := s.SetMergeIntent(ctx, "c1", "bob"); err != nil {
		t.Fatalf("SetMergeIntent (re-label): %v", err)
	}
	mi, err := s.GetMergeIntent(ctx, "c1")
	if err != nil || mi == nil || mi.DispatchedHead != "abc123" {
		t.Fatalf("GetMergeIntent after re-label = %+v, err=%v; want dispatched_head still abc123", mi, err)
	}
	if mi.RequestedBy != "bob" {
		t.Errorf("RequestedBy = %q; want the re-label to update it to bob", mi.RequestedBy)
	}
}

// Run with -race: MaxOpenConns(1) must prevent SQLITE_BUSY under concurrent writers, or every
// best-effort log-and-continue caller silently drops writes under load.
func TestStoreConcurrentAccessNoErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const goroutines = 20
	const opsPerGoroutine = 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*opsPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				chatID := fmt.Sprintf("chat-%d", g%5) // keys collide across goroutines on purpose
				if err := s.SetSnapshot(ctx, chatID, fmt.Sprintf(`{"n":%d}`, i)); err != nil {
					errs <- fmt.Errorf("SetSnapshot: %w", err)
				}
				if _, _, err := s.GetSnapshot(ctx, chatID); err != nil {
					errs <- fmt.Errorf("GetSnapshot: %w", err)
				}
				if err := s.SetMergeIntent(ctx, chatID, "bot"); err != nil {
					errs <- fmt.Errorf("SetMergeIntent: %w", err)
				}
				if _, err := s.GetMergeIntent(ctx, chatID); err != nil {
					errs <- fmt.Errorf("GetMergeIntent: %w", err)
				}
				if err := s.DeleteMergeIntent(ctx, chatID); err != nil {
					errs <- fmt.Errorf("DeleteMergeIntent: %w", err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// mergeIfApproved (Set) racing tryMerge (Get-then-Delete) for one chat could double-merge;
// keyedMutex serializes every consume-if-present pass against every set for that chat.
func TestKeyedMutexPreventsDoubleConsumeMergeIntent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	var km keyedMutex
	const chatID = "github-acme-widgets-42"

	const rounds = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	consumed := map[string]int{} // requestedBy nonce -> times observed as non-nil by a consumer

	// setter: stands up a fresh intent each round.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			nonce := fmt.Sprintf("nonce-%d", i)
			unlock := km.Lock(chatID)
			err := s.SetMergeIntent(ctx, chatID, nonce)
			unlock()
			if err != nil {
				t.Errorf("SetMergeIntent: %v", err)
			}
		}
	}()

	// two concurrent consumers: mimics mergeIfApproved and
	// tryMerge both potentially firing for the same PR.
	for c := 0; c < 2; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				unlock := km.Lock(chatID)
				mi, err := s.GetMergeIntent(ctx, chatID)
				if err != nil {
					unlock()
					t.Errorf("GetMergeIntent: %v", err)
					continue
				}
				if mi != nil {
					if err := s.DeleteMergeIntent(ctx, chatID); err != nil {
						t.Errorf("DeleteMergeIntent: %v", err)
					}
				}
				unlock()
				if mi != nil {
					mu.Lock()
					consumed[mi.RequestedBy]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	for nonce, n := range consumed {
		if n > 1 {
			t.Errorf("nonce %q consumed %d times, want at most once (double-consume race not closed)", nonce, n)
		}
	}
}

// TestStoreLifecycle pins the SDK contract: the Factory touches nothing on disk
// (server validate runs it against a throwaway dir), Start opens and migrates, Close closes.
func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	keyPEM, _ := testKeyPEM(t)
	dir := t.TempDir()
	raw := fmt.Sprintf("client_id: Iv1.test\nwebhook_secret: s\nprivate_key: %q\n", keyPEM)
	ext, err := factory(sdk.Host{DataDir: dir}, []byte(raw))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("factory wrote %d entries to DataDir, want none", len(ents))
	}

	e := ext.(*Extension)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "github.sqlite")); err != nil {
		t.Fatalf("Start did not create the store: %v", err)
	}
	if err := e.store.SetMergeIntentDispatchedHead(ctx, "c1", "abc"); err != nil {
		t.Fatalf("migrated schema missing dispatched_head: %v", err)
	}

	e.Wait()
	if err := e.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := e.store.GetMergeIntent(ctx, "c1"); !errors.Is(err, errStoreClosed) {
		t.Fatalf("query after Close: err=%v, want errStoreClosed", err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A query before Start (RunEnded from a node resumed at boot) opens the store itself.
func TestStoreOpensOnFirstUse(t *testing.T) {
	s := newStore(t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetSnapshot(context.Background(), "c1", "{}"); err != nil {
		t.Fatalf("SetSnapshot before Start: %v", err)
	}
}

// A failed open is not cached: once the directory exists, the next query opens the store.
func TestStoreRetriesAfterFailedOpen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "missing")
	s := newStore(dir)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetSnapshot(ctx, "c1", "{}"); err == nil {
		t.Fatal("SetSnapshot with a missing DataDir succeeded, want an open error")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.conn(); err != nil {
		t.Fatalf("conn after MkdirAll: %v", err)
	}
	if err := s.SetSnapshot(ctx, "c1", "{}"); err != nil {
		t.Fatalf("SetSnapshot after retry: %v", err)
	}
	if _, ok, err := s.GetSnapshot(ctx, "c1"); err != nil || !ok {
		t.Fatalf("GetSnapshot after retry: ok=%v err=%v", ok, err)
	}
}

// Start prunes pending runs older than lease + run timeout and keeps fresh ones.
func TestStartPrunesStalePendingRuns(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SetPendingRun(ctx, PendingRunRow{ChatID: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.exec(ctx, `INSERT INTO github_pending_run (chat_id, session_id, owner, repo, number, is_pr, login, is_plan, is_label_trigger, comment_id, default_branch, installation_id, clone_url, created_at)
		VALUES ('stale', '', '', '', 0, 0, '', 0, 0, 0, '', 0, '', ?)`, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	e := &Extension{store: st, runTimeout: time.Minute}
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for id, want := range map[string]bool{"fresh": true, "stale": false} {
		row, err := st.GetPendingRun(ctx, id)
		if err != nil || (row != nil) != want {
			t.Errorf("GetPendingRun(%q) = %+v, %v; want kept=%v", id, row, err, want)
		}
	}
}

// Start restores a surviving pending run's claim, so a trigger during the resumed run is deduped, not doubled.
func TestStartRestoresPendingRunClaims(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SetPendingRun(ctx, PendingRunRow{ChatID: "c1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	e := &Extension{store: st, runTimeout: time.Hour}
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, _, claimed := e.claimInflight("s1"); claimed {
		t.Error("a new trigger claimed s1 while its resumed run is still pending")
	}
	if _, _, claimed := e.claimInflight("s2"); !claimed {
		t.Error("s2 has no pending run, so its trigger must claim")
	}
}
