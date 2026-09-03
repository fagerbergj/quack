package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/ledger"
)

// fakeLedger is a minimal, in-memory ledger.LedgerStore double for the WAL
// hook tests (#1100): AppendIntent allocates a gapless per-chat seq exactly
// like PGStore's real transaction, and failNext forces the next AppendIntent
// to fail closed without touching the entry log.
type fakeLedger struct {
	mu       sync.Mutex
	seqs     map[string]int64
	entries  map[string][]ledger.Entry
	failNext bool
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{seqs: map[string]int64{}, entries: map[string][]ledger.Entry{}}
}

func (f *fakeLedger) Append(context.Context, string, []byte) error { return nil }
func (f *fakeLedger) ReadStream(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeLedger) List(context.Context) ([]ledger.SessionRef, error) { return nil, nil }
func (f *fakeLedger) Delete(context.Context, string) error              { return nil }

func (f *fakeLedger) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return 0, errors.New("fakeLedger: forced AppendIntent failure")
	}
	f.seqs[e.ChatID]++
	e.Seq = f.seqs[e.ChatID]
	e.At = time.Now().UTC()
	f.entries[e.ChatID] = append(f.entries[e.ChatID], e)
	return e.Seq, nil
}

func (f *fakeLedger) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledger.Entry
	for _, e := range f.entries[chatID] {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

type doc struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// hintIdentity: instance = hint verbatim - stands in for a subject-identity
// kind like code_review (its instance comes from outside the content).
func hintIdentity(_ []byte, hint string) (string, error) { return hint, nil }

func init() {
	Register("test.structured", KindSpec{
		Class: Structured,
		Validate: func(raw json.RawMessage) error {
			var d doc
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.A == "" {
				return errors.New("a is required")
			}
			return nil
		},
		Identity: hintIdentity,
	})
	Register("test.blob", KindSpec{Class: Blob, Identity: hintIdentity})
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return New(artifact.InMemoryService(), "quack", "user1", "chat1")
}

func TestKindOf(t *testing.T) {
	if got := KindOf("code_review:pr:123"); got != "code_review" {
		t.Fatalf("KindOf = %q, want code_review", got)
	}
	// instance itself contains the separator - KindOf must still isolate the kind.
	if got := KindOf("finding:3f9a2c1e"); got != "finding" {
		t.Fatalf("KindOf(finding) = %q", got)
	}
}

func TestSaveStructuredRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev, err := c.SaveStructured(ctx, "test.structured", doc{A: "x", B: 1}, "main", Lineage{NodeID: "n1", Round: 1})
	if err != nil || rev != 1 || id != "test.structured:main" {
		t.Fatalf("SaveStructured: id=%q rev=%d err=%v", id, rev, err)
	}
	raw, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
	if err != nil || !ok || gotRev != 1 {
		t.Fatalf("LatestWithMeta: raw=%s rev=%d ok=%v err=%v", raw, gotRev, ok, err)
	}
	// InMemoryService has no row to persist lineage on (#1090 known ceiling) -
	// zero value, not an error.
	if lineage.NodeID != "" {
		t.Fatalf("expected zero lineage over InMemoryService, got %+v", lineage)
	}
}

func TestSaveStructuredRejectsInvalidBody(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "", B: 1}, "main", Lineage{}); err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if _, _, ok, _ := c.Latest(ctx, "test.structured:main"); ok {
		t.Fatal("a rejected save must not persist a revision")
	}
}

func TestSaveStructuredRejectsWrongClass(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "test.blob", doc{A: "x"}, "main", Lineage{}); err == nil {
		t.Fatal("expected error saving a blob kind via SaveStructured")
	}
}

func TestSaveBlobAcceptsAnyMime(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, _, err := c.SaveBlob(ctx, "test.blob", []byte("# hi"), "text/markdown", "doc:1", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "test.blob:doc:1" {
		t.Fatalf("id = %q", id)
	}
	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "# hi" {
		t.Fatalf("raw=%s ok=%v err=%v", raw, ok, err)
	}
}

func TestSaveUnregisteredKindFails(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "nope", doc{A: "x"}, "main", Lineage{}); err == nil {
		t.Fatal("expected error for an unregistered kind")
	}
}

func TestIdentityForMatchesSave(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	wantID, err := IdentityFor("test.structured", doc{A: "x"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "x"}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if wantID != gotID {
		t.Fatalf("IdentityFor = %q, Save derived %q", wantID, gotID)
	}
}

func TestLatestMissing(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	_, _, ok, err := c.Latest(ctx, "test.blob:nope")
	if err != nil || ok {
		t.Fatalf("expected no record, got ok=%v err=%v", ok, err)
	}
}

// TestEveryRevisionKept proves design V4.1 #2: no retention call exists, so
// every save keeps its own revision and Latest always reports the newest.
func TestEveryRevisionKept(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	var id string
	for i := 0; i < 5; i++ {
		var err error
		id, _, err = c.SaveBlob(ctx, "test.blob", []byte{byte(i)}, "text/plain", "doc:1", Lineage{})
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, rev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || rev != 5 {
		t.Fatalf("Latest: raw=%v rev=%d ok=%v err=%v", raw, rev, ok, err)
	}
	if got := raw[0]; got != 4 {
		t.Fatalf("Latest returned revision content %d, want the 5th save's byte 4", got)
	}
}

// TestSameContentDifferentHintSameID: a content-hashed kind's identity
// ignores hint, matching the finding requirement (same finding, any node).
func TestContentHashIgnoresHint(t *testing.T) {
	Register("test.hashed", KindSpec{
		Class: Blob,
		Identity: func(content []byte, _ string) (string, error) {
			return string(content), nil // trivial "hash" for the test
		},
	})
	ctx := context.Background()
	c := newTestClient(t)
	id1, _, err := c.SaveBlob(ctx, "test.hashed", []byte("same"), "text/plain", "node-a", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := c.SaveBlob(ctx, "test.hashed", []byte("same"), "text/plain", "node-b", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("same content under different hints produced different ids: %q vs %q", id1, id2)
	}
}

// TestWALAppendFailureBlocksRowWrite is V4 §7 case 11: an AppendIntent
// failure must leave the store row unwritten and return the error - never a
// partial write.
func TestWALAppendFailureBlocksRowWrite(t *testing.T) {
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	fl.failNext = true
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte("v1"), "text/plain", "doc:wal", Lineage{})
	if err == nil {
		t.Fatalf("SaveBlob succeeded despite a forced WAL append failure: id=%q rev=%d", id, rev)
	}
	// No row should exist: Latest on the id the save would have used.
	wantID, idErr := IdentityFor("test.blob", nil, "doc:wal")
	if idErr != nil {
		t.Fatal(idErr)
	}
	if _, _, ok, lerr := c.Latest(ctx, wantID); lerr != nil || ok {
		t.Fatalf("Latest after failed WAL append: ok=%v err=%v, want ok=false", ok, lerr)
	}
}

// TestWALParentRevisionReadFromLedger is V4 §7's parent_revision contract
// (#1090 §4.9): the second save's parent_revision must come from the
// ledger's own artifact.revision entry, not be recomputed by the caller.
func TestWALParentRevisionReadFromLedger(t *testing.T) {
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, rev1, err := c.SaveBlob(ctx, "test.blob", []byte("v1"), "text/plain", "doc:parent", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if rev1 != 1 {
		t.Fatalf("first save revision = %d, want 1", rev1)
	}
	// Deliberately pass a WRONG ParentRevision (as if the caller's own
	// tracking drifted) - the ledger's own read must still win.
	id2, rev2, err := c.SaveBlob(ctx, "test.blob", []byte("v2"), "text/plain", "doc:parent", Lineage{ParentRevision: 99})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id || rev2 != 2 {
		t.Fatalf("second save: id=%q rev=%d, want id=%q rev=2", id2, rev2, id)
	}
	entries, err := fl.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawParent2 bool
	for _, e := range entries {
		if e.Key != id {
			continue
		}
		var p artifactRevisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Revision == 2 {
			sawParent2 = true
			if p.ParentRevision != 1 {
				t.Fatalf("revision 2's parent_revision = %d, want 1 (read from the ledger, not the caller's 99)", p.ParentRevision)
			}
		}
	}
	if !sawParent2 {
		t.Fatal("no artifact.revision entry for revision 2 found in the ledger")
	}
}

// TestNoLedgerConfiguredUnchanged is V4 §7's "with no ledger, nothing
// changes" requirement: WithLedger never called, save behaves exactly as
// before #1100 (no error, no ledger dependency).
func TestNoLedgerConfiguredUnchanged(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	_, rev, err := c.SaveBlob(ctx, "test.blob", []byte("v1"), "text/plain", "doc:no-ledger", Lineage{})
	if err != nil || rev != 1 {
		t.Fatalf("SaveBlob with no ledger configured: rev=%d err=%v, want rev=1 err=nil", rev, err)
	}
}

// TestConcurrentSaveSameIDWALRevisionsAreSequential is the adversarial-review
// fix for #1100: N goroutines racing SaveBlob on the SAME id must produce WAL
// artifact.revision entries numbered exactly 1..N with a strictly increasing
// parent chain (each entry's parent_revision == the previous one's revision),
// and each WAL revision must equal what the store itself assigned - proving
// read-parent + AppendIntent + saveRow run under one lock, not just that
// AppendIntent's own seq is gapless. Run with -race.
func TestConcurrentSaveSameIDWALRevisionsAreSequential(t *testing.T) {
	const n = 20
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := c.SaveBlob(ctx, "test.blob", []byte{byte(i)}, "text/plain", "doc:race", Lineage{})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("save %d failed: %v", i, err)
		}
	}

	id, err := IdentityFor("test.blob", nil, "doc:race")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fl.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var revs []int
	byRev := map[int]artifactRevisionPayload{}
	for _, e := range entries {
		if e.Key != id {
			continue
		}
		var p artifactRevisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, p.Revision)
		byRev[p.Revision] = p
	}
	if len(revs) != n {
		t.Fatalf("got %d artifact.revision WAL entries for %s, want %d (one per save, no phantom/duplicate revisions)", len(revs), id, n)
	}
	sort.Ints(revs)
	for i, r := range revs {
		want := i + 1
		if r != want {
			t.Fatalf("WAL revisions = %v, want exactly 1..%d with no gaps or duplicates", revs, n)
		}
		if want > 1 && byRev[want].ParentRevision != want-1 {
			t.Fatalf("revision %d's parent_revision = %d, want %d (strictly increasing chain)", want, byRev[want].ParentRevision, want-1)
		}
	}
	_, storeRev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if storeRev != n {
		t.Fatalf("store's own latest revision = %d, want %d (matches the WAL's highest)", storeRev, n)
	}
}
