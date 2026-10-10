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

// fakeLedger is MemStore whose next AppendIntent can be forced to fail closed.
type fakeLedger struct {
	*ledgertest.MemStore
	failNext bool
}

func newFakeLedger() *fakeLedger { return &fakeLedger{MemStore: ledgertest.NewMemStore()} }

func (f *fakeLedger) AppendIntent(ctx context.Context, e ledger.Entry) (int64, error) {
	if f.failNext {
		f.failNext = false
		return 0, errors.New("fakeLedger: forced AppendIntent failure")
	}
	return f.MemStore.AppendIntent(ctx, e)
}

type doc struct {
	A string `json:"a"`
	B int    `json:"b"`
	C string `json:"c,omitempty"` // second string field, for tests needing two independent leaves to edit
}

// hintIdentity: instance = hint verbatim - stands in for a subject-identity
// kind like code_review (its instance comes from outside the content).
func hintIdentity(_ []byte, hint string) (string, error) { return hint, nil }

func init() {
	Register("test.structured", KindSpec{
		Class:      Structured,
		JSONSchema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"string"}}}`,
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
	// Registered in init(), not the test body, so -count>1 can't panic on a duplicate Register.
	Register("test.hashed", KindSpec{
		Class: Blob,
		Identity: func(content []byte, _ string) (string, error) {
			return string(content), nil // trivial "hash" for the test
		},
	})
	Register("test.system", KindSpec{Class: Blob, Identity: hintIdentity, System: true})
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return New(artifact.InMemoryService(), "quack", "user1", "chat1")
}

// metaAwareInMemory adds SaveWithMeta/LoadWithMeta over artifact.InMemoryService(), so a test can read back
// real lineage without a database.
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
	// InMemoryService has no row to persist lineage on: zero value, not an error.
	if lineage.NodeID != "" {
		t.Fatalf("expected zero lineage over InMemoryService, got %+v", lineage)
	}
}

// TestLoadVersionWithMeta_RoundTrip: a specific past revision's own lineage
// comes back, not the latest's - DependencyArtifact's per-revision scan relies on this.
func TestLoadVersionWithMeta_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c := New(newMetaAwareInMemory(), "quack", "user1", "chat1")
	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte("v1 content"), "text/plain", "main", Lineage{NodeID: "n1", Round: 1})
	if err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	data, lineage, ok, err := c.LoadVersionWithMeta(ctx, id, rev)
	if err != nil || !ok || string(data) != "v1 content" {
		t.Fatalf("LoadVersionWithMeta: data=%q ok=%v err=%v", data, ok, err)
	}
	if lineage.NodeID != "n1" || lineage.Round != 1 {
		t.Errorf("lineage = %+v, want NodeID=n1 Round=1", lineage)
	}
}

// TestLoadVersionWithMeta_NotFound: a missing id/version is ok=false, not an error.
func TestLoadVersionWithMeta_NotFound(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	_, _, ok, err := c.LoadVersionWithMeta(ctx, "test.blob:nonexistent", 1)
	if err != nil || ok {
		t.Fatalf("LoadVersionWithMeta on a missing id: ok=%v err=%v, want ok=false err=nil", ok, err)
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

// TestListPopulatesSavedAt pins List's ArtifactSummary.SavedAt to the
// backing lineage, the field a caller ranks multiple ids of one kind by.
func TestListPopulatesSavedAt(t *testing.T) {
	ctx := context.Background()
	svc := newMetaAwareInMemory()
	c := New(svc, "quack", "user1", "chat1")
	older := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().UTC().Truncate(time.Second)
	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("a"), "text/plain", "doc:a", Lineage{SavedAt: older}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("b"), "text/plain", "doc:b", Lineage{SavedAt: newer}); err != nil {
		t.Fatal(err)
	}
	summaries, err := c.List(ctx, "test.blob")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]time.Time{}
	for _, s := range summaries {
		got[s.ID] = s.SavedAt
	}
	if !got["test.blob:doc:a"].Equal(older) || !got["test.blob:doc:b"].Equal(newer) {
		t.Fatalf("List SavedAt = %v, want doc:a=%v doc:b=%v", got, older, newer)
	}
}

// TestEveryRevisionKept: no retention call exists, so
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
	// Old/New are plain decoded text, not raw JSON syntax - the edit targets
	// field a's value, found by decoding the document, not by matching `"a":"..."`.
	rev2, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: "world"}}, Lineage{})
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

// TestEditRejectsSystemKind: tryEdit must refuse a System kind - the one
// place both the native and MCP edit_artifact surfaces route through.
func TestEditRejectsSystemKind(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev, err := c.SaveBlob(ctx, "test.system", []byte("original"), "text/plain", "hint", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev, []EditOp{{Old: "original", New: "forged"}}, Lineage{}); err == nil {
		t.Fatal("Edit on a System kind should be refused")
	}
	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "original" {
		t.Fatalf("Latest: raw=%q ok=%v err=%v, want the content unchanged", raw, ok, err)
	}
}

// TestEditStaleBaseMergesWhenUnique: a stale base_revision still succeeds when its `old` snippets match
// uniquely against the newer latest (a non-intersecting concurrent edit).
func TestEditStaleBaseMergesWhenUnique(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1, C: "seed"}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent editor edits field c while this caller still thinks rev1 is latest.
	rev2, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "seed", New: "changed"}}, Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// This caller's base_revision (rev1) is now stale, but its edit targets
	// a region the concurrent edit never touched - must still succeed.
	rev3, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: "world"}}, Lineage{})
	if err != nil {
		t.Fatalf("Edit with stale base should merge, got: %v", err)
	}
	if rev3 != rev2+1 {
		t.Fatalf("revision = %d, want %d", rev3, rev2+1)
	}
	var d doc
	if err := json.Unmarshal(merged, &d); err != nil || d.A != "world" || d.C != "changed" {
		t.Fatalf("merged = %s (want both edits applied), err=%v", merged, err)
	}
}

// TestEditStaleBaseConflictsWhenIntersecting: a stale edit whose `old` region the newer revision changed
// fails with the current latest, and writes nothing.
func TestEditStaleBaseConflictsWhenIntersecting(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: "changed"}}, Lineage{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: "conflicting"}}, Lineage{})
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

// TestEditRejectsSchemaInvalidResult: the splice runs before spec.Validate, so an edit producing an invalid
// record must still be rejected without writing a revision.
func TestEditRejectsSchemaInvalidResult(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: ""}}, Lineage{})
	if err == nil {
		t.Fatal("Edit producing a schema-invalid record should fail")
	}
	var conflict *EditConflict
	if errors.As(err, &conflict) {
		t.Fatalf("Edit = %v, want a validation error, not *EditConflict", err)
	}
	if !strings.Contains(err.Error(), "fails validation") {
		t.Fatalf("Edit error = %v, want it to mention validation", err)
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, id); latestRev != rev1 {
		t.Fatalf("failed edit must not write - latest revision = %d, want %d", latestRev, rev1)
	}
}

// TestEditRejectsNegativeBaseRevision: a negative base_revision is rejected.
func TestEditRejectsNegativeBaseRevision(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, -1, []EditOp{{Old: "hello", New: "world"}}, Lineage{}); err == nil {
		t.Fatal("Edit with base_revision -1 should fail")
	}
}

// TestEditRejectsBaseRevisionAboveLatest: a base_revision above the actual latest names a revision that
// doesn't exist and must error.
func TestEditRejectsBaseRevisionAboveLatest(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev+5, []EditOp{{Old: "hello", New: "world"}}, Lineage{}); err == nil {
		t.Fatal("Edit with base_revision above latest should fail")
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, id); latestRev != rev {
		t.Fatalf("rejected edit must not write - latest revision = %d, want %d", latestRev, rev)
	}
}

// TestEditRecordsBaseRevisionInLineage: base_revision lands in the written lineage, distinct from
// parent_revision (the latest the merge targeted) when the two differ.
func TestEditRecordsBaseRevisionInLineage(t *testing.T) {
	ctx := context.Background()
	// newTestClient's InMemoryService lacks metaSaver/metaLoader, so LatestWithMeta would return no lineage.
	c := New(newMetaAwareInMemory(), "quack", "user1", "chat1")
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1, C: "seed"}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	rev2, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "seed", New: "changed"}}, Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// Stale base (rev1) merges against the real latest (rev2).
	if _, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: "hello", New: "world"}}, Lineage{}); err != nil {
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

// TestIdentityForMatchesWriteID: the id a write returns equals what IdentityFor derives for the same
// content/hint.
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

// TestWALAppendFailureBlocksRowWrite: an AppendIntent failure leaves the row unwritten and returns the error.
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

// TestWALParentRevisionReadFromLedger: the second save's parent_revision comes from the ledger's own
// artifact.revision entry, not recomputed by the caller.
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

// TestNoLedgerConfiguredUnchanged: without WithLedger, saves work with no ledger dependency.
func TestNoLedgerConfiguredUnchanged(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	_, rev, err := c.SaveBlob(ctx, "test.blob", []byte("v1"), "text/plain", "doc:no-ledger", Lineage{})
	if err != nil || rev != 1 {
		t.Fatalf("SaveBlob with no ledger configured: rev=%d err=%v, want rev=1 err=nil", rev, err)
	}
}

// TestIdenticalContentSaveSkipsRevisionAndWAL: re-saving the latest's exact bytes mints no revision and
// appends no WAL intent; a revise that changed nothing must look like nothing happened.
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

// TestRevertGetsNewRevision: A→B→A lands as revision 3 with content "A" rather than collapsing into the old
// revision, while a genuine retry at the same parent still collapses.
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

// TestConcurrentSaveSameIDWALRevisionsAreSequential: racing saves on one id yield WAL revisions 1..N with a
// strict parent chain, each matching the store's own revision. Run with -race.
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

// failOnceSaveService fails its Nth Save (1-indexed), then delegates: a saveRow failure after the WAL
// entry was already appended.
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

// TestSaveRowFailureAfterAppendSelfHeals: a saveRow failure after a WAL append leaves the parent claimed
// with no row; a later plain save adopts the orphaned claim at the exact revision it reserved.
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

// TestSaveRetryAfterPartialSave_CompletesOrphanedDuplicate: a duplicate-key hit doesn't prove a row exists,
// so a same-content retry must complete the orphaned intent, never report success without a row.
func TestSaveRetryAfterPartialSave_CompletesOrphanedDuplicate(t *testing.T) {
	svc := &failOnceSaveService{Service: artifact.InMemoryService(), failCall: 1}
	ls := ledgertest.NewMemStore()
	c := New(svc, "quack", "user1", "chat1").WithLedger(ls)
	ctx := context.Background()

	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("same"), "text/plain", "doc:dup-orphan", Lineage{}); err == nil {
		t.Fatal("expected the first save (forced saveRow failure) to error")
	}
	// Identical content hits the crashed attempt's idempotency key before any ErrStaleParent/adopt.
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

// TestSaveRetryAfterPartialSave_ForeignAdoptionFailsClosed: another writer may adopt the orphaned slot with
// different bytes; the original writer's retry must get a named mismatch error, never that content.
func TestSaveRetryAfterPartialSave_ForeignAdoptionFailsClosed(t *testing.T) {
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
	// Writer A retries with its ORIGINAL content: same idempotency key as
	// its crashed attempt, but revision 1 now belongs to writer B.
	if _, _, err := c.SaveBlob(ctx, "test.blob", []byte("a-content"), "text/plain", "doc:foreign-adopt", Lineage{}); err == nil {
		t.Fatal("writer A's retry succeeded despite revision 1 holding writer B's content")
	} else if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("writer A's retry error = %v, want it to name the content mismatch", err)
	}
	// Revision 1 still holds writer B's content - never silently clobbered.
	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "b-content" {
		t.Fatalf("Latest after the failed-closed retry = %q ok=%v err=%v, want writer B's content untouched", raw, ok, err)
	}
}

// TestRegisterPanicsOnInvalidJSONSchema: an unparseable JSONSchema fails at registration, so both
// write_<kind> tool surfaces fail the same way instead of one skipping silently.
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

// TestRegisterPanicsOnEmptySchemaForStructuredKind: an empty schema on a structured kind is rejected at
// registration (the "" allowance is for blob kinds only).
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

// TestGateVsEditNoSilentOverwrite: a gate save and an Edit targeting the same parent never silently
// overwrite each other; one claim wins and the other gets ledger.ErrStaleParent.
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

// TestGateSaveEditWALConcurrentSingleLock: racing gate saves and Edits on one id leave a WAL chain with no
// lost, duplicate or out-of-order revisions, and the store's latest matches the WAL. Run with -race.
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
			// An EditConflict is fine under this race: the invariant is chain integrity, not that every edit lands.
			_, _, _ = c.Edit(ctx, id, 1, []EditOp{{Old: "seed", New: "edited"}}, Lineage{})
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
