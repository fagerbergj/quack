package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGStore starts a real Postgres container - AppendIntent's seq
// allocation relies on a real UPSERT's row locking, which sqlite/mocks
// can't exercise. Skips (not fails) when Docker isn't reachable, matching
// internal/store's own container tests.
func newTestPGStore(t *testing.T) *PGStore {
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
	store, err := NewPGStore(db)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
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

// TestPGStoreExporterRoundTripUnchanged is verification case (b): the OTel
// exporter's best-effort Append/ReadStream path behaves the same against
// the postgres backend as it does against FSStore's JSONL file.
func TestPGStoreExporterRoundTripUnchanged(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	ctx := context.Background()
	const chatID = "chat-otel"

	lines := [][]byte{
		[]byte(`{"body":"first"}`),
		[]byte(`{"body":"second"}`),
	}
	exp := NewExporter(store)
	if err := exp.store.Append(ctx, chatID, lines[0]); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := exp.store.Append(ctx, chatID, lines[1]); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	rc, err := store.ReadStream(ctx, chatID)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// jsonb round-trips the VALUE, not the byte-for-byte text (whitespace
	// gets reformatted), so compare decoded lines rather than raw bytes.
	got := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(got) != len(lines) {
		t.Fatalf("got %d lines, want %d: %q", len(got), len(lines), got)
	}
	for i, line := range lines {
		var want, have map[string]any
		if err := json.Unmarshal(line, &want); err != nil {
			t.Fatalf("bad fixture line %d: %v", i, err)
		}
		if err := json.Unmarshal([]byte(got[i]), &have); err != nil {
			t.Fatalf("line %d not valid JSON: %v (%q)", i, err, got[i])
		}
		if want["body"] != have["body"] {
			t.Errorf("line %d = %v, want %v", i, have, want)
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
		seq, err := store.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindArtifactRevision, Key: "code_review:pr:1"})
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

// TestPGStoreReadStreamUnknownChatIsErrNotExist: a chat with zero rows must
// look like FSStore's missing file to callers checking errors.Is(err,
// fs.ErrNotExist) - GetChatRecording's 404 depends on this, not on the
// stream merely being empty.
func TestPGStoreReadStreamUnknownChatIsErrNotExist(t *testing.T) {
	t.Parallel()
	store := newTestPGStore(t)
	_, err := store.ReadStream(context.Background(), "never-recorded")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadStream unknown chat err = %v, want fs.ErrNotExist", err)
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
