package vetting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// versionedMetaInMemory is metaAwareInMemory's sibling (reviewrecord_test.go),
// keyed by (id, version) rather than just id - DependencyArtifact reads a
// specific PAST revision's own lineage, which a latest-only map can't give it.
type versionedMetaInMemory struct {
	artifact.Service
	mu        sync.Mutex
	loadCalls int
	meta      map[string]struct {
		kind, class string
		lineage     []byte
	}
}

func newVersionedMetaInMemory() *versionedMetaInMemory {
	return &versionedMetaInMemory{Service: artifact.InMemoryService(), meta: map[string]struct {
		kind, class string
		lineage     []byte
	}{}}
}

func versionedMetaKey(appName, userID, sessionID, fileName string, version int64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", appName, userID, sessionID, fileName, version)
}

func (m *versionedMetaInMemory) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error) {
	resp, err := m.Service.Save(ctx, req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[versionedMetaKey(req.AppName, req.UserID, req.SessionID, req.FileName, resp.Version)] = struct {
		kind, class string
		lineage     []byte
	}{kind, class, lineageJSON}
	return resp, nil
}

func (m *versionedMetaInMemory) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error) {
	resp, err := m.Service.Load(ctx, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadCalls++
	if err != nil {
		return nil, "", "", nil, err
	}
	meta := m.meta[versionedMetaKey(req.AppName, req.UserID, req.SessionID, req.FileName, req.Version)]
	return resp, meta.kind, meta.class, meta.lineage, nil
}

func (m *versionedMetaInMemory) LoadCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadCalls
}

// testSystemKind stands in for a real System kind (e.g. tools' web_page,
// registered by a package vetting's tests don't import) to prove
// DependencyArtifact excludes System kinds without needing that import.
const testSystemKind = "vtest_system_kind"

func init() {
	recordstore.Register(testSystemKind, recordstore.KindSpec{
		Class:    recordstore.Blob,
		Identity: func(_ []byte, hint string) (string, error) { return hint, nil },
		System:   true,
	})
}

// TestDependencyArtifact_NoArtifactsService: recordClient's own nil guard -
// a chat with no artifact.Service configured has nothing to inline, ever.
func TestDependencyArtifact_NoArtifactsService(t *testing.T) {
	_, _, content, ok := DependencyArtifact(context.Background(), Config{ChatID: "chat1"}, "dep")
	if ok || content != "" {
		t.Errorf("DependencyArtifact = (%q, ok=%v), want (\"\", false) with no Artifacts service", content, ok)
	}
}

// TestDependencyArtifact_TypedKindScopedByLineage pins the #1504 review's
// blocker: the typed kind's id is chat-scoped (documentHint has no nodeID), so
// a sibling's LATER write under the same id must never win over dep's own,
// earlier one.
func TestDependencyArtifact_TypedKindScopedByLineage(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte("dep's own document"), "text/markdown", "doc:chat1",
		recordstore.Lineage{NodeID: "dep", Round: 1}); err != nil {
		t.Fatalf("seed dep's revision: %v", err)
	}
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte("a sibling's later document"), "text/markdown", "doc:chat1",
		recordstore.Lineage{NodeID: "sibling", Round: 2}); err != nil {
		t.Fatalf("seed sibling's revision: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", Artifact: "document"}

	id, rev, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || content != "dep's own document" || rev != 1 {
		t.Errorf("DependencyArtifact = (id=%q, rev=%d, content=%q, ok=%v), want dep's own rev 1, not the sibling's later one", id, rev, content, ok)
	}
}

// TestDependencyArtifact_NewerTurnBeatsAnOlderTurnsHigherRound pins the round
// 2 review's B2: an id is chat-scoped across every TURN, not just this one -
// an earlier turn's round 8 must never outrank this turn's round 1, the
// newest revision by save order regardless of what round number it carries.
func TestDependencyArtifact_NewerTurnBeatsAnOlderTurnsHigherRound(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte("an earlier turn's round 8 answer"), "text/markdown", "dep",
		recordstore.Lineage{NodeID: "dep", Round: 8}); err != nil {
		t.Fatalf("seed the earlier turn's revision: %v", err)
	}
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte("this turn's round 1 answer"), "text/markdown", "dep",
		recordstore.Lineage{NodeID: "dep", Round: 1}); err != nil {
		t.Fatalf("seed this turn's revision: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1"}

	_, _, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || content != "this turn's round 1 answer" {
		t.Errorf("DependencyArtifact content = %q, ok=%v, want this turn's newer revision, not the older turn's higher-round one", content, ok)
	}
}

// TestDependencyArtifact_StopsAtTheFirstMatchPerCandidate proves the newest-
// first scan loads at most one revision per candidate when dep's own write is
// already the newest - it never walks the id's whole history.
func TestDependencyArtifact_StopsAtTheFirstMatchPerCandidate(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	for i, author := range []string{"someone-else", "someone-else", "dep"} {
		if _, _, err := c.SaveBlob(context.Background(), "text", []byte("revision"), "text/markdown", "dep",
			recordstore.Lineage{NodeID: author, Round: i + 1}); err != nil {
			t.Fatalf("seed revision %d: %v", i+1, err)
		}
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1"}

	_, rev, _, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || rev != 3 {
		t.Fatalf("DependencyArtifact rev=%d ok=%v, want dep's own newest revision 3", rev, ok)
	}
	if got := svc.LoadCalls(); got != 1 {
		t.Errorf("LoadWithMeta calls = %d, want 1 - the older, non-matching revisions must never be loaded", got)
	}
}

// TestDependencyArtifact_GenericTextFallback covers the "text:<dep>" identity
// every gate writes for a node with no configured Artifact kind.
func TestDependencyArtifact_GenericTextFallback(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte("the episodic round answer"), "text/markdown", "dep",
		recordstore.Lineage{NodeID: "dep", Round: 1}); err != nil {
		t.Fatalf("seed text record: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1"}

	_, _, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || content != "the episodic round answer" {
		t.Errorf("DependencyArtifact = (%q, ok=%v), want the text record", content, ok)
	}
}

// TestDependencyArtifact_SystemKindNeverACandidate: a System kind is excluded
// even when its lineage matches and outranks the fallback.
func TestDependencyArtifact_SystemKindNeverACandidate(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), testSystemKind, []byte("fetched evidence, not a deliverable"), "text/markdown", "dep-system",
		recordstore.Lineage{NodeID: "dep", Round: 5}); err != nil {
		t.Fatalf("seed system kind: %v", err)
	}
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte("the real episodic answer"), "text/markdown", "dep",
		recordstore.Lineage{NodeID: "dep", Round: 1}); err != nil {
		t.Fatalf("seed text record: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", Artifact: testSystemKind}

	_, _, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || content != "the real episodic answer" {
		t.Errorf("DependencyArtifact = (%q, ok=%v), want the text fallback - the higher-round System kind must be skipped", content, ok)
	}
}

// TestDependencyArtifact_NothingMatchesLineage: an id exists, but every
// revision belongs to a different node - nothing of dep's own to inline.
func TestDependencyArtifact_NothingMatchesLineage(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte("someone else's document"), "text/markdown", "doc:chat1",
		recordstore.Lineage{NodeID: "someone-else", Round: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", Artifact: "document"}

	_, _, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if ok || content != "" {
		t.Errorf("DependencyArtifact = (%q, ok=%v), want (\"\", false) - no revision is dep's own", content, ok)
	}
}

// TestDependencyArtifact_ReviewerKind covers the cfg.IsReviewer selector -
// a reviewer dependency's own code_review record, not the text fallback.
func TestDependencyArtifact_ReviewerKind(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	hint := SubjectHint("chat1")
	if _, _, err := c.SaveStructured(context.Background(), kindCodeReview, CodeReviewRecord{Verdict: "approve"}, hint,
		recordstore.Lineage{NodeID: "dep", Round: 1}); err != nil {
		t.Fatalf("seed code_review: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", IsReviewer: true}

	id, _, _, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if !ok || !strings.HasPrefix(id, kindCodeReview+":") {
		t.Errorf("DependencyArtifact = (id=%q, ok=%v), want the reviewer's own code_review record", id, ok)
	}
}

// failingVersionsService errors every Versions call, so DependencyArtifact's
// own "skip a candidate whose revision listing failed" branch is exercised.
type failingVersionsService struct{ artifact.Service }

func (failingVersionsService) Versions(context.Context, *artifact.VersionsRequest) (*artifact.VersionsResponse, error) {
	return nil, errors.New("simulated versions listing failure")
}

// TestDependencyArtifact_VersionsErrorSkipsCandidate: a candidate whose
// revision listing errors is skipped, not treated as fatal for the whole call.
func TestDependencyArtifact_VersionsErrorSkipsCandidate(t *testing.T) {
	svc := failingVersionsService{Service: newVersionedMetaInMemory()}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", Artifact: "document"}

	_, _, content, ok := DependencyArtifact(context.Background(), cfg, "dep")
	if ok || content != "" {
		t.Errorf("DependencyArtifact = (%q, ok=%v), want (\"\", false) when every candidate's Versions call fails", content, ok)
	}
}
