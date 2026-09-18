package dag

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// versionedMetaInMemory implements recordstore's optional SaveWithMeta/
// LoadWithMeta over artifact.InMemoryService(), keyed by (id, version) - a
// production Postgres-backed store keeps a past revision's own lineage, which
// this stands in for so DependencyArtifact's per-revision scan is actually exercised.
type versionedMetaInMemory struct {
	artifact.Service
	mu   sync.Mutex
	meta map[string]struct {
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
	if err != nil {
		return nil, "", "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := m.meta[versionedMetaKey(req.AppName, req.UserID, req.SessionID, req.FileName, req.Version)]
	return resp, meta.kind, meta.class, meta.lineage, nil
}

// saveDepRevision seeds one artifact revision with explicit lineage, the same
// shape a gate's saveEpisodicRound/tool-write produces.
func saveDepRevision(t *testing.T, c *recordstore.Client, kind, hint, content, nodeID string, round int) {
	t.Helper()
	if _, _, err := c.SaveBlob(context.Background(), kind, []byte(content), "text/markdown", hint,
		recordstore.Lineage{NodeID: nodeID, Round: round}); err != nil {
		t.Fatalf("seed %s round %d for %s: %v", kind, round, nodeID, err)
	}
}

// depPlan: a two-node plan where "synth" depends on "dep".
func depPlan(depArtifact, depAgent string) Plan {
	return Plan{
		Nodes: []Node{
			{ID: "dep", AgentName: depAgent, Artifact: depArtifact},
			{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"dep"}, Task: "Write the report."},
		},
	}
}

// TestBuildTask_AppendsDependencyArtifactAfterTheAnswer pins prod chat
// effc2636: a researcher answered "fixed in artifact revision 3", and its
// dependent must see BOTH that answer AND the full document, appended, not swapped for it.
func TestBuildTask_AppendsDependencyArtifactAfterTheAnswer(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	full := strings.Repeat("full findings, paragraph by paragraph. ", 20)
	saveDepRevision(t, c, "document", "doc:chat1", full, "dep", 1)
	plan := depPlan("document", "web-researcher")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "fixed in artifact revision 3"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, "fixed in artifact revision 3") {
		t.Errorf("dependent prompt lost the dependency's own answer:\n%s", got)
	}
	if !strings.Contains(got, "full findings, paragraph by paragraph.") {
		t.Errorf("dependent prompt missing the dependency's full document:\n%s", got)
	}
	if !strings.Contains(got, "dep's full artifact - document:doc:chat1 revision 1") {
		t.Errorf("dependent prompt missing the artifact id/revision header:\n%s", got)
	}
}

// TestBuildTask_SiblingsSharingAnAgentEachGetTheirOwnArtifact pins the #1504
// review's blocker: the typed kind's id is chat-scoped, so two sibling
// researcher nodes writing "document" collide on the same id - buildTask must
// still give each dependent only ITS OWN dep's artifact.
func TestBuildTask_SiblingsSharingAnAgentEachGetTheirOwnArtifact(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	saveDepRevision(t, c, "document", "doc:chat1", "dep-a's own document body", "dep-a", 1)
	saveDepRevision(t, c, "document", "doc:chat1", "dep-b's own document body", "dep-b", 1)
	plan := Plan{Nodes: []Node{
		{ID: "dep-a", AgentName: "web-researcher", Artifact: "document"},
		{ID: "dep-b", AgentName: "web-researcher", Artifact: "document"},
		{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"dep-a", "dep-b"}, Task: "Write the report."},
	}}
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep-a": "pointer a", "dep-b": "pointer b"}

	got := buildTask(context.Background(), plan, plan.Nodes[2], upstream, nil, cfg)
	blocks := strings.Split(got, "\n\n---\n\n")
	if len(blocks) < 2 {
		t.Fatalf("expected a block per dependency, got:\n%s", got)
	}
	if !strings.Contains(blocks[0], "dep-a's own document body") || strings.Contains(blocks[0], "dep-b's own document body") {
		t.Errorf("dep-a's block should carry only dep-a's own document, got:\n%s", blocks[0])
	}
	if !strings.Contains(blocks[1], "dep-b's own document body") || strings.Contains(blocks[1], "dep-a's own document body") {
		t.Errorf("dep-b's block should carry only dep-b's own document, got:\n%s", blocks[1])
	}
}

// TestBuildTask_NewerTurnBeatsAnOlderTurnsHigherRound: the text:<dep> id is
// chat-scoped across every turn, not just this one - an earlier turn's round
// 8 must never outrank this turn's round 1.
func TestBuildTask_NewerTurnBeatsAnOlderTurnsHigherRound(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	saveDepRevision(t, c, "text", "dep", "an earlier turn's round 8 answer", "dep", 8)
	saveDepRevision(t, c, "text", "dep", "this turn's round 1 answer", "dep", 1)
	plan := depPlan("", "code-explorer")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "the delivered answer"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, "this turn's round 1 answer") {
		t.Errorf("dependent prompt should carry this turn's newer revision:\n%s", got)
	}
	if strings.Contains(got, "an earlier turn's round 8 answer") {
		t.Errorf("dependent prompt leaked an older turn's stale, higher-round revision:\n%s", got)
	}
}

// TestBuildTask_ArtifactEqualToAnswerAppendsNothing: the common text:<dep>
// case, where the artifact IS the answer - nothing extra to append.
func TestBuildTask_ArtifactEqualToAnswerAppendsNothing(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	answer := "this is the node's whole answer, saved as its own text record too"
	saveDepRevision(t, c, "text", "dep", answer, "dep", 1)
	plan := depPlan("", "code-explorer")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": answer}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if strings.Contains(got, "full artifact") {
		t.Errorf("dependent prompt appended a block for an artifact identical to the answer:\n%s", got)
	}
	if strings.Count(got, answer) != 1 {
		t.Errorf("answer should appear exactly once, got %d times:\n%s", strings.Count(got, answer), got)
	}
}

// TestBuildTask_KeepsAnswerWhenNoArtifact: no Artifacts service configured -
// the answer text is all the dependent sees.
func TestBuildTask_KeepsAnswerWhenNoArtifact(t *testing.T) {
	plan := depPlan("document", "web-researcher")
	upstream := map[string]string{"dep": "the only thing this node produced"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, vetting.Config{})

	if !strings.Contains(got, "the only thing this node produced") {
		t.Errorf("dependent prompt should keep the answer text when there is no artifact:\n%s", got)
	}
	if strings.Contains(got, "full artifact") {
		t.Errorf("dependent prompt appended a block with no artifact to append:\n%s", got)
	}
}

// TestBuildTask_CapTruncatesWithReadArtifactMarker proves an oversized
// artifact is capped to the dependent node's own budget, with a trailing
// marker naming the id so the node can read_artifact the rest.
func TestBuildTask_CapTruncatesWithReadArtifactMarker(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	huge := strings.Repeat("x", 1_000_000)
	saveDepRevision(t, c, "document", "doc:chat1", huge, "dep", 1)
	plan := depPlan("document", "web-researcher")
	// ContextWindow 100 tokens * 40% share * 4 bytes/token = a 160-byte budget.
	plan.Nodes[1].ContextWindow = 100
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "pointer"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, `[... truncated; read_artifact("document:doc:chat1") for the rest]`) {
		t.Errorf("dependent prompt missing the truncation marker naming the artifact id:\n%s", got)
	}
	if strings.Contains(got, huge) {
		t.Errorf("dependent prompt inlined the full 1MB artifact instead of capping it")
	}
	if got := artifactByteBudget(plan.Nodes[1]); got != 160 {
		t.Fatalf("artifactByteBudget(ContextWindow=100) = %d, want 160", got)
	}
}

// TestBuildTask_BudgetExhaustedByAnEarlierDependencyAppendsNothingMore: two
// deps each with their own artifact, but the first already spends the whole budget.
func TestBuildTask_BudgetExhaustedByAnEarlierDependencyAppendsNothingMore(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	saveDepRevision(t, c, "text", "dep-a", strings.Repeat("a", 200), "dep-a", 1)
	saveDepRevision(t, c, "text", "dep-b", "dep-b's own artifact", "dep-b", 1)
	plan := Plan{Nodes: []Node{
		{ID: "dep-a", AgentName: "code-explorer"},
		{ID: "dep-b", AgentName: "code-explorer"},
		{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"dep-a", "dep-b"}, Task: "Write the report.", ContextWindow: 100},
	}}
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep-a": "pointer a", "dep-b": "pointer b"}

	got := buildTask(context.Background(), plan, plan.Nodes[2], upstream, nil, cfg)

	if strings.Contains(got, "dep-b's own artifact") {
		t.Errorf("dep-b's artifact should not be appended once dep-a's spent the whole budget:\n%s", got)
	}
}

// TestBuildTask_HeaderAndMarkerBytesCountTowardBudget: dep-a's content alone
// fits under the budget (so appendDependencyArtifact never truncates it), but
// its header text pushes the actual bytes spent over budget - dep-b must get
// nothing once that overspend is counted.
func TestBuildTask_HeaderAndMarkerBytesCountTowardBudget(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	saveDepRevision(t, c, "text", "dep-a", strings.Repeat("a", 180), "dep-a", 1)
	saveDepRevision(t, c, "text", "dep-b", "dep-b's own artifact", "dep-b", 1)
	plan := Plan{Nodes: []Node{
		{ID: "dep-a", AgentName: "code-explorer"},
		{ID: "dep-b", AgentName: "code-explorer"},
		// 125 tokens * 40% * 4 bytes/token = a 200-byte budget: dep-a's 180-byte
		// content fits alone, but content + its ~50-byte header does not.
		{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"dep-a", "dep-b"}, Task: "Write the report.", ContextWindow: 125},
	}}
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep-a": "pointer a", "dep-b": "pointer b"}

	got := buildTask(context.Background(), plan, plan.Nodes[2], upstream, nil, cfg)

	if !strings.Contains(got, strings.Repeat("a", 180)) {
		t.Errorf("dep-a's own content should still be appended whole (it fits alone):\n%s", got)
	}
	if strings.Contains(got, "dep-b's own artifact") {
		t.Errorf("dep-b's artifact should not fit once dep-a's header/marker bytes count against the budget:\n%s", got)
	}
}

// TestSafeTruncateBytes covers safeTruncateBytes' own boundary cases directly -
// buildTask only ever calls it once content is already known to exceed budget.
func TestSafeTruncateBytes(t *testing.T) {
	if got := safeTruncateBytes("hello", -1); got != "" {
		t.Errorf("safeTruncateBytes(negative n) = %q, want \"\"", got)
	}
	if got := safeTruncateBytes("hello", 100); got != "hello" {
		t.Errorf("safeTruncateBytes(n >= len) = %q, want the content unchanged", got)
	}
	// "日" is 3 bytes; cutting at 2 bytes ("x" + 1 of "日"'s 3 bytes) must back off
	// to the last full rune, not split it.
	if got := safeTruncateBytes("x日本語", 2); got != "x" {
		t.Errorf("safeTruncateBytes mid-rune = %q, want %q", got, "x")
	}
}

// TestBuildTask_ArtifactInliningKeepsGateFailedWarning proves the vetting
// warning still precedes the appended content.
func TestBuildTask_ArtifactInliningKeepsGateFailedWarning(t *testing.T) {
	svc := newVersionedMetaInMemory()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	full := strings.Repeat("unverified findings at length. ", 20)
	saveDepRevision(t, c, "document", "doc:chat1", full, "dep", 1)
	plan := depPlan("document", "web-researcher")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "short pointer"}
	gateFailed := map[string]bool{"dep": true}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, gateFailed, cfg)

	if !strings.Contains(got, "FAILED independent quality vetting") {
		t.Errorf("gate-failed warning missing once the dependency's artifact is appended:\n%s", got)
	}
	if !strings.Contains(got, "unverified findings at length.") {
		t.Errorf("dependent prompt missing the appended artifact alongside the warning:\n%s", got)
	}
	if strings.Index(got, "short pointer") > strings.Index(got, "unverified findings at length.") {
		t.Errorf("the answer must come before the appended artifact block:\n%s", got)
	}
}
