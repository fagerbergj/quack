package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
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

func (f *fakeLedger) List(context.Context) ([]ledger.SessionRef, error) { return nil, nil }
func (f *fakeLedger) Delete(context.Context, string) error              { return nil }

func (f *fakeLedger) MaxSeq(_ context.Context, chatID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seqs[chatID], nil
}

// AppendIntent enforces the same (chat_id, key, parent_revision) and
// idempotency_key uniqueness the real stores do (#1144 P4), so a test using
// fakeLedger exercises saveAt's retry/no-op paths the same way MemStore or
// PGStore would.
func (f *fakeLedger) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return 0, errors.New("fakeLedger: forced AppendIntent failure")
	}
	// Idempotency checked BEFORE parent-conflict, matching MemStore/PGStore
	// (#1237 review: the reverse order here meant a same-content retry never
	// exercised the DuplicateIntentError branch at all).
	if e.IdempotencyKey != "" {
		for _, ex := range f.entries[e.ChatID] {
			if ex.IdempotencyKey == e.IdempotencyKey {
				return 0, &ledger.DuplicateIntentError{Existing: ex}
			}
		}
	}
	if e.Kind == ledger.KindArtifactRevision {
		var p struct {
			ParentRevision int64 `json:"parent_revision"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		for _, ex := range f.entries[e.ChatID] {
			if ex.Kind != ledger.KindArtifactRevision || ex.Key != e.Key {
				continue
			}
			var exp struct {
				ParentRevision int64 `json:"parent_revision"`
			}
			_ = json.Unmarshal(ex.Payload, &exp)
			if exp.ParentRevision == p.ParentRevision {
				return 0, ledger.ErrStaleParent
			}
		}
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
		Class:      Structured,
		JSONSchema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}}}`,
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

// metaAwareInMemory implements the optional SaveWithMeta/LoadWithMeta pair
// over artifact.InMemoryService() - production always wraps a store that
// supports these (internal/store.TurnAwareService); this stands in for that
// so a test can read back real lineage without a database.
type metaAwareInMemory struct {
	artifact.Service
	mu   sync.Mutex
	meta map[string][]byte
}

func newMetaAwareInMemory() *metaAwareInMemory {
	return &metaAwareInMemory{Service: artifact.InMemoryService(), meta: map[string][]byte{}}
}

func metaKey(appName, userID, sessionID, fileName string) string {
	return appName + "\x00" + userID + "\x00" + sessionID + "\x00" + fileName
}

func (m *metaAwareInMemory) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error) {
	resp, err := m.Service.Save(ctx, req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)] = lineageJSON
	return resp, nil
}

func (m *metaAwareInMemory) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error) {
	resp, err := m.Service.Load(ctx, req)
	if err != nil {
		return nil, "", "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lineageJSON := m.meta[metaKey(req.AppName, req.UserID, req.SessionID, req.FileName)]
	return resp, "", "", lineageJSON, nil
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
	raw, _, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
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

// TestEditDirectApply covers the base_revision-current case: edits apply straight to the latest content.
func TestEditDirectApply(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	rev2, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if rev2 != rev1+1 {
		t.Fatalf("revision = %d, want %d", rev2, rev1+1)
	}
	var d doc
	if err := json.Unmarshal(merged, &d); err != nil || d.A != "world" {
		t.Fatalf("merged = %s, err=%v", merged, err)
	}
}

// TestEditStaleBaseMergesWhenUnique covers V4 §7 case 5: a stale
// base_revision still succeeds when its `old` snippets still match uniquely
// against the newer latest content (a non-intersecting concurrent edit).
func TestEditStaleBaseMergesWhenUnique(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent editor bumps B while this caller still thinks rev1 is latest.
	rev2, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"b":1`, New: `"b":2`}}, Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// This caller's base_revision (rev1) is now stale, but its edit targets
	// a region the concurrent edit never touched - must still succeed.
	rev3, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{})
	if err != nil {
		t.Fatalf("Edit with stale base should merge, got: %v", err)
	}
	if rev3 != rev2+1 {
		t.Fatalf("revision = %d, want %d", rev3, rev2+1)
	}
	var d doc
	if err := json.Unmarshal(merged, &d); err != nil || d.A != "world" || d.B != 2 {
		t.Fatalf("merged = %s (want both edits applied), err=%v", merged, err)
	}
}

// TestEditStaleBaseConflictsWhenIntersecting covers V4 §7 case 5's failure
// half: a stale edit whose `old` region was itself changed by the newer
// revision no longer matches, and the call fails with the current latest -
// no partial write.
func TestEditStaleBaseConflictsWhenIntersecting(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"changed"`}}, Lineage{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"conflicting"`}}, Lineage{})
	var conflict *EditConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("Edit = %v, want *EditConflict", err)
	}
	if conflict.Revision != rev1+1 {
		t.Fatalf("conflict.Revision = %d, want %d", conflict.Revision, rev1+1)
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, id); latestRev != rev1+1 {
		t.Fatalf("failed edit must not write - latest revision = %d, want %d", latestRev, rev1+1)
	}
}

// TestEditRejectsNegativeBaseRevision covers #1091 adversarial review
// suggestion #3: base_revision was accepted but never validated.
func TestEditRejectsNegativeBaseRevision(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, -1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{}); err == nil {
		t.Fatal("Edit with base_revision -1 should fail")
	}
}

// TestEditRejectsBaseRevisionAboveLatest covers #1091 adversarial review
// suggestion #3: a base_revision greater than the actual latest revision is
// nonsensical (it names a revision that doesn't exist yet) and must error
// rather than silently proceeding.
func TestEditRejectsBaseRevisionAboveLatest(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev+5, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{}); err == nil {
		t.Fatal("Edit with base_revision above latest should fail")
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, id); latestRev != rev {
		t.Fatalf("rejected edit must not write - latest revision = %d, want %d", latestRev, rev)
	}
}

// TestEditRecordsBaseRevisionInLineage covers #1091 adversarial review
// suggestion #3: base_revision is no longer silently dropped - it lands in
// the written revision's lineage, distinct from parent_revision (the real
// latest the merge targeted) whenever the two differ (a merge, not a direct apply).
func TestEditRecordsBaseRevisionInLineage(t *testing.T) {
	ctx := context.Background()
	// LatestWithMeta only returns real lineage when the wrapped service
	// implements metaSaver/metaLoader (InMemoryService, used by newTestClient,
	// doesn't) - mirror internal/vetting's metaAwareInMemory test double.
	c := New(newMetaAwareInMemory(), "quack", "user1", "chat1")
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	rev2, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"b":1`, New: `"b":2`}}, Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// Stale base (rev1) merges against the real latest (rev2).
	if _, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{}); err != nil {
		t.Fatalf("Edit with stale base should merge, got: %v", err)
	}
	_, _, lineage, _, ok, err := c.LatestWithMeta(ctx, id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if lineage.BaseRevision != rev1 {
		t.Fatalf("lineage.BaseRevision = %d, want the caller's base_revision %d", lineage.BaseRevision, rev1)
	}
	if lineage.ParentRevision != rev2 {
		t.Fatalf("lineage.ParentRevision = %d, want the real latest merged against (%d)", lineage.ParentRevision, rev2)
	}
}

// TestEditNonUniqueMatchFails: an `old` matching 0 or 2+ times fails without
// writing, even against the current latest revision (no stale base involved).
func TestEditNonUniqueMatchFails(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	blobID, brev, err := c.SaveBlob(ctx, "test.blob", []byte("aa"), "text/plain", "b1", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, blobID, brev, []EditOp{{Old: "a", New: "z"}}, Lineage{}); err == nil {
		t.Fatal("Edit with a 2x-matching old should fail")
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, blobID); latestRev != brev {
		t.Fatalf("failed edit must not write - latest revision = %d, want %d", latestRev, brev)
	}
}

// TestWriteFindingOffSchemaFailsWithoutWriting: an off-schema structured
// write is rejected by the registry's Validate and nothing is persisted.
func TestWriteFindingOffSchemaFailsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	// test.structured requires non-empty "a" - the write_<kind> equivalent
	// for this suite's stand-in kind.
	if _, _, err := c.SaveStructured(ctx, "test.structured", map[string]any{"b": 1}, "bad", Lineage{}); err == nil {
		t.Fatal("expected validation failure for a body missing the required field")
	}
	if _, _, ok, _ := c.Latest(ctx, "test.structured:bad"); ok {
		t.Fatal("an off-schema write must not persist a revision")
	}
}

// TestIdentityForMatchesWriteID: the id a write returns equals what
// IdentityFor (the registry's own Identity function) derives for the same
// content/hint - required by V4 §7 case 5.
func TestIdentityForMatchesWriteID(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	d := doc{A: "x", B: 1}
	wantID, err := IdentityFor("test.structured", d, "main")
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := c.SaveStructured(ctx, "test.structured", d, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("SaveStructured id = %q, want IdentityFor's %q", gotID, wantID)
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

// TestIdenticalContentSaveSkipsRevisionAndWAL is #1123's no-op-save guard: a
// second save with byte-identical content to the current latest revision
// must NOT mint revision 2 (still at 1) and must NOT append a WAL
// artifact.revision intent for the skipped attempt - a failed revise that
// couldn't actually change anything must look like nothing happened, not
// like an aborted or empty revision.
func TestIdenticalContentSaveSkipsRevisionAndWAL(t *testing.T) {
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, rev1, err := c.SaveBlob(ctx, "test.blob", []byte("same content"), "text/plain", "doc:noop", Lineage{})
	if err != nil || rev1 != 1 {
		t.Fatalf("first SaveBlob: rev=%d err=%v, want rev=1 err=nil", rev1, err)
	}

	id2, rev2, err := c.SaveBlob(ctx, "test.blob", []byte("same content"), "text/plain", "doc:noop", Lineage{})
	if err != nil {
		t.Fatalf("second (identical) SaveBlob returned an error: %v", err)
	}
	if id2 != id || rev2 != 1 {
		t.Fatalf("second (identical) SaveBlob: id=%q rev=%d, want id=%q rev=1 (no new revision)", id2, rev2, id)
	}

	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "same content" {
		t.Fatalf("Latest after no-op save: raw=%q ok=%v err=%v", raw, ok, err)
	}

	entries, err := fl.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var revisionEntries int
	for _, e := range entries {
		if e.Key == id && e.Kind == ledger.KindArtifactRevision {
			revisionEntries++
		}
	}
	if revisionEntries != 1 {
		t.Fatalf("WAL has %d artifact.revision entries for %s, want exactly 1 (the skipped identical save must append none)", revisionEntries, id)
	}
}

// TestRevertGetsNewRevision is the finding-2 fix: idempotencyKey used to hash
// only (id, data), so a save matching ANY earlier revision - not just the
// latest - collapsed into a no-op reporting that old revision, silently
// dropping the write. A→B→A must land as revision 3 with content "A", while a
// genuine retry at the SAME parent still collapses.
func TestRevertGetsNewRevision(t *testing.T) {
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, rev1, err := c.SaveBlob(ctx, "test.blob", []byte("A"), "text/plain", "doc:revert", Lineage{})
	if err != nil || rev1 != 1 {
		t.Fatalf("save A: rev=%d err=%v, want rev=1 err=nil", rev1, err)
	}
	_, rev2, err := c.SaveBlob(ctx, "test.blob", []byte("B"), "text/plain", "doc:revert", Lineage{})
	if err != nil || rev2 != 2 {
		t.Fatalf("save B: rev=%d err=%v, want rev=2 err=nil", rev2, err)
	}
	_, rev3, err := c.SaveBlob(ctx, "test.blob", []byte("A"), "text/plain", "doc:revert", Lineage{})
	if err != nil {
		t.Fatalf("save A again returned an error: %v", err)
	}
	if rev3 != 3 {
		t.Fatalf("save A again: rev=%d, want rev=3 (a revert must get its own new revision)", rev3)
	}
	raw, latestRev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || latestRev != 3 || string(raw) != "A" {
		t.Fatalf("Latest after revert: raw=%q rev=%d ok=%v err=%v, want %q rev=3", raw, latestRev, ok, err, "A")
	}

	// A genuine retry at the SAME parent (revision 3, content "A") must still
	// collapse into a no-op, not mint revision 4.
	_, rev4, err := c.SaveBlob(ctx, "test.blob", []byte("A"), "text/plain", "doc:revert", Lineage{})
	if err != nil {
		t.Fatalf("duplicate retry at the same parent returned an error: %v", err)
	}
	if rev4 != 3 {
		t.Fatalf("duplicate retry at the same parent: rev=%d, want rev=3 (still collapses)", rev4)
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

// failOnceSaveService fails its Nth Save call (1-indexed), then behaves like
// the wrapped service - stands in for a saveRow failure that lands AFTER a
// WAL artifact.revision entry has already been appended (#1100 review
// finding: the wedge case).
type failOnceSaveService struct {
	artifact.Service
	failCall int
	calls    int
}

func (s *failOnceSaveService) Save(ctx context.Context, req *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	s.calls++
	if s.calls == s.failCall {
		return nil, errors.New("failOnceSaveService: forced failure")
	}
	return s.Service.Save(ctx, req)
}

// TestSaveRowFailureAfterAppendSelfHeals is #1144 P4's cheap fix for the
// #1100 wedge case: a saveRow failure right after a successful WAL append
// leaves that parent claimed with no row - the store-level unique index
// replaces the old best-effort (and losable) artifact.revision.aborted
// marker, and instead of self-healing via a compensating entry, a PLAIN
// SAVE on the same id later adopts the orphaned claim (saveAtOrAdopt) and
// completes it at the exact revision the orphan reserved. No extra bytes are
// ever duplicated into the ledger for this.
func TestSaveRowFailureAfterAppendSelfHeals(t *testing.T) {
	svc := &failOnceSaveService{Service: artifact.InMemoryService(), failCall: 1}
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("v1"), "text/plain", "doc:wedge", Lineage{}); err == nil {
		t.Fatal("expected the first save (forced saveRow failure) to error")
	}
	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte("v1-retry"), "text/plain", "doc:wedge", Lineage{})
	if err != nil {
		t.Fatalf("save after the wedge should self-heal by adopting the orphan, got: %v", err)
	}
	if rev != 1 {
		t.Fatalf("adopted revision = %d, want 1 (the orphan's own reserved revision)", rev)
	}
	raw, storeRev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || storeRev != 1 || string(raw) != "v1-retry" {
		t.Fatalf("Latest after self-heal = %q rev=%d ok=%v err=%v, want the adopting save's content at revision 1", raw, storeRev, ok, err)
	}
}

// TestSaveRetryAfterPartialSave_CompletesOrphanedDuplicate is the #1237
// review fix: the ledger's IdempotencyKey is committed by AppendIntent
// BEFORE saveRow, so a duplicate-key hit alone does not prove a row exists -
// the ORIGINAL save could have crashed between the two. A same-content retry
// must never report success without a row; it must complete the orphaned
// intent (same as saveAtOrAdopt does for a stale-parent orphan) instead of
// lying about having saved.
func TestSaveRetryAfterPartialSave_CompletesOrphanedDuplicate(t *testing.T) {
	svc := &failOnceSaveService{Service: artifact.InMemoryService(), failCall: 1}
	// ledgertest.NewMemStore(), not the local fakeLedger double: MemStore checks
	// idempotency before parent-conflict, same order as PGStore, so this
	// actually exercises the DuplicateIntentError branch under test (#1237
	// review: the fake's old, reversed check order meant it never did).
	ls := ledgertest.NewMemStore()
	c := New(svc, "quack", "user1", "chat1").WithLedger(ls)
	ctx := context.Background()

	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("same"), "text/plain", "doc:dup-orphan", Lineage{}); err == nil {
		t.Fatal("expected the first save (forced saveRow failure) to error")
	}
	// Identical content: the retry's AppendIntent hits the SAME idempotency
	// key the crashed attempt already committed, before ever reaching
	// ErrStaleParent/adopt.
	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte("same"), "text/plain", "doc:dup-orphan", Lineage{})
	if err != nil {
		t.Fatalf("retry with identical content should complete the orphaned duplicate, got: %v", err)
	}
	if rev != 1 {
		t.Fatalf("rev = %d, want 1", rev)
	}
	raw, storeRev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || storeRev != 1 || string(raw) != "same" {
		t.Fatalf("Latest after the retry = %q rev=%d ok=%v err=%v, want the row actually written at revision 1", raw, storeRev, ok, err)
	}
}

// TestSaveRetryAfterPartialSave_ForeignAdoptionAdvancesPastIt is the
// finding-2 counterpart of the #1237 orphan case: once writer B has adopted
// the orphaned slot, writer A's "retry" recomputes its parent against the
// NEW tip (B's revision), so it's no longer the same write attempt - it's a
// fresh save that lands its own new revision, same as any other writer
// showing up after B. Revision 1 (B's content) is never clobbered; A's
// content lands as revision 2, not lost and not silently mislabeled as B's.
func TestSaveRetryAfterPartialSave_ForeignAdoptionAdvancesPastIt(t *testing.T) {
	svc := &failOnceSaveService{Service: artifact.InMemoryService(), failCall: 1}
	ls := ledgertest.NewMemStore()
	c := New(svc, "quack", "user1", "chat1").WithLedger(ls)
	ctx := context.Background()

	// Writer A partial-saves: AppendIntent lands, saveRow crashes.
	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("a-content"), "text/plain", "doc:foreign-adopt", Lineage{}); err == nil {
		t.Fatal("expected writer A's first save (forced saveRow failure) to error")
	}
	// Writer B adopts the orphaned slot with DIFFERENT content.
	id, bRev, err := c.SaveBlob(ctx, "test.blob", []byte("b-content"), "text/plain", "doc:foreign-adopt", Lineage{})
	if err != nil || bRev != 1 {
		t.Fatalf("writer B's adopting save: rev %d, %v, want it to adopt revision 1", bRev, err)
	}
	// Writer A retries with its ORIGINAL content: the tip has moved to
	// writer B's revision 1, so this lands as its own revision 2 instead of
	// colliding with A's stale, orphaned intent.
	_, aRev, err := c.SaveBlob(ctx, "test.blob", []byte("a-content"), "text/plain", "doc:foreign-adopt", Lineage{})
	if err != nil {
		t.Fatalf("writer A's retry after B's adoption should land as a new revision, got: %v", err)
	}
	if aRev != 2 {
		t.Fatalf("writer A's retry: rev=%d, want rev=2 (B's revision 1 is untouched)", aRev)
	}
	// Revision 1 still holds writer B's content - never silently clobbered.
	if raw, ok, err := c.LoadVersion(ctx, id, 1); err != nil || !ok || string(raw) != "b-content" {
		t.Fatalf("revision 1 = %q ok=%v err=%v, want writer B's content untouched", raw, ok, err)
	}
	if raw, rev, ok, err := c.Latest(ctx, id); err != nil || !ok || rev != 2 || string(raw) != "a-content" {
		t.Fatalf("Latest = %q rev=%d ok=%v err=%v, want writer A's content at revision 2", raw, rev, ok, err)
	}
}

// TestRegisterPanicsOnInvalidJSONSchema: the single root-cause fix for #1108
// finding 3 - a kind with unparseable JSONSchema must fail loudly at
// registration time (like the existing missing-Identity/duplicate-kind
// panics), not be silently skipped by whichever tool-generation surface
// (MCP write_<kind>, ADK write_<kind>) happens to notice later. One guard
// here means both surfaces get it for free.
func TestRegisterPanicsOnInvalidJSONSchema(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on an invalid JSONSchema")
		}
		msg, _ := r.(string)
		if !containsAll(msg, "test_bad_schema_kind", "invalid JSONSchema") {
			t.Fatalf("panic message = %q, want it to name the kind and the problem", msg)
		}
	}()
	Register("test_bad_schema_kind", KindSpec{
		Class:      Structured,
		JSONSchema: `{not valid json`,
		Identity:   func(_ []byte, hint string) (string, error) { return hint, nil },
	})
}

// TestRegisterPanicsOnEmptySchemaForStructuredKind: #1108 L1 - an empty
// JSONSchema on a structured kind used to pass Register (the "" guard was
// meant for blob kinds, which have no schema), then made MCP silently skip
// the write_<kind> tool while ADK hard-failed the whole run for the same
// registry state. Reject it at registration instead, same as an invalid one.
func TestRegisterPanicsOnEmptySchemaForStructuredKind(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on a structured kind with an empty JSONSchema")
		}
		msg, _ := r.(string)
		if !containsAll(msg, "test_empty_schema_kind", "without a JSONSchema") {
			t.Fatalf("panic message = %q, want it to name the kind and the problem", msg)
		}
	}()
	Register("test_empty_schema_kind", KindSpec{
		Class:    Structured,
		Identity: func(_ []byte, hint string) (string, error) { return hint, nil },
	})
}

// TestGateVsEditNoSilentOverwrite replaces TestEditVsGateSaveSerializes
// (#1108 finding 1). idLocks used to block a gate save until Edit's in-flight
// write finished; #1144 P4 deletes that lock in favor of the ledger's unique
// (chat_id, key, parent_revision) index - a gate save that targets the SAME
// parent Edit is about to write no longer blocks, but it also can never
// silently overwrite Edit's result: exactly one of the two claims wins that
// parent, the other gets ledger.ErrStaleParent instead of vanishing.
func TestGateVsEditNoSilentOverwrite(t *testing.T) {
	fl := ledgertest.NewMemStore()
	c := New(artifact.InMemoryService(), "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 0}, "race-id", Lineage{})
	if err != nil {
		t.Fatal(err)
	}

	_, editErr := c.saveAt(ctx, id, "test.structured", Structured, "application/json", []byte(`{"a":"edited","b":0}`), Lineage{}, rev1)
	_, gateErr := c.saveAt(ctx, id, "test.structured", Structured, "application/json", []byte(`{"a":"gate","b":99}`), Lineage{}, rev1)

	var wins, conflicts int
	for _, err := range []error{editErr, gateErr} {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ledger.ErrStaleParent):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly one winner and one ledger.ErrStaleParent (no silent overwrite)", wins, conflicts)
	}
}

// TestGateSaveEditWALConcurrentSingleLock is #1107: gate saves (SaveStructured)
// and Edit used to serialize through two different locks (recordstore's own
// idLocks plus a second map guarding only the WAL section of saveLocked).
// Racing both against the same id under one consolidated lock must still
// produce a WAL chain with no lost/duplicate/out-of-order revisions and a
// store latest revision matching the WAL's highest - the invariant both old
// locks existed to protect. Run with -race.
func TestGateSaveEditWALConcurrentSingleLock(t *testing.T) {
	const n = 15
	svc := artifact.InMemoryService()
	fl := newFakeLedger()
	c := New(svc, "quack", "user1", "chat1").WithLedger(fl)
	ctx := context.Background()

	id, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "seed", B: 0}, "race-both", Lineage{})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, _ = c.SaveStructured(ctx, "test.structured", doc{A: "gate", B: i + 1}, "race-both", Lineage{})
		}(i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// A conflict (EditConflict, the old text already overwritten by a
			// concurrent gate save) is an acceptable outcome under this race -
			// the invariant under test is chain integrity, not that every
			// edit lands.
			_, _, _ = c.Edit(ctx, id, 1, []EditOp{{Old: `"a":"seed"`, New: `"a":"edited"`}}, Lineage{})
		}()
	}
	close(start)
	wg.Wait()

	entries, err := fl.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var revs []int
	byRev := map[int]artifactRevisionPayload{}
	for _, e := range entries {
		if e.Key != id || e.Kind != ledger.KindArtifactRevision {
			continue
		}
		var p artifactRevisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, p.Revision)
		byRev[p.Revision] = p
	}
	sort.Ints(revs)
	for i, r := range revs {
		want := i + 1
		if r != want {
			t.Fatalf("WAL revisions = %v, want exactly 1..%d with no gaps or duplicates", revs, len(revs))
		}
		if want > 1 && byRev[want].ParentRevision != want-1 {
			t.Fatalf("revision %d's parent_revision = %d, want %d (strictly increasing chain)", want, byRev[want].ParentRevision, want-1)
		}
	}
	_, storeRev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if storeRev != len(revs) {
		t.Fatalf("store's own latest revision = %d, want %d (matches the WAL's highest)", storeRev, len(revs))
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
