package dag

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// depPlan: a two-node plan where "synth" depends on "dep", matching buildTask's
// dependencyContent lookup (plan.Nodes by ID, dep's Artifact/AgentName).
func depPlan(depArtifact, depAgent string) Plan {
	return Plan{
		Nodes: []Node{
			{ID: "dep", AgentName: depAgent, Artifact: depArtifact},
			{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"dep"}, Task: "Write the report."},
		},
	}
}

// TestBuildTask_InlinesDependencyDocumentWhenLongerThanAnswer pins prod chat
// effc2636: a researcher answered "fixed in artifact revision 3" and its
// dependent must see the FULL document, not just that pointer.
func TestBuildTask_InlinesDependencyDocumentWhenLongerThanAnswer(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	full := strings.Repeat("full findings, paragraph by paragraph. ", 20)
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte(full), "text/markdown", "doc:chat1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	plan := depPlan("document", "web-researcher")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "fixed in artifact revision 3"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, "full findings, paragraph by paragraph.") {
		t.Errorf("dependent prompt missing the dependency's full document; got a pointer instead of the deliverable:\n%s", got)
	}
	if strings.Contains(got, "fixed in artifact revision 3") {
		t.Errorf("dependent prompt still carries the short pointer answer instead of the full document:\n%s", got)
	}
}

// TestBuildTask_InlinesDependencyGenericTextRecord covers the fallback id
// "text:<nodeID>" every gate writes (reviewrecord.go's saveTextRound) for a
// node with no configured Artifact kind and no IsReviewer selector.
func TestBuildTask_InlinesDependencyGenericTextRecord(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	full := strings.Repeat("the long episodic answer text. ", 20)
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte(full), "text/markdown", "dep", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed text record: %v", err)
	}
	plan := depPlan("", "code-explorer")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "short summary"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, "the long episodic answer text.") {
		t.Errorf("dependent prompt missing the dependency's generic text: record:\n%s", got)
	}
}

// TestBuildTask_KeepsAnswerWhenNoArtifact pins today's behaviour unchanged: no
// Artifacts service, or nothing saved for the dependency, means the answer
// text is what the dependent sees - exactly as before this change.
func TestBuildTask_KeepsAnswerWhenNoArtifact(t *testing.T) {
	plan := depPlan("document", "web-researcher")
	upstream := map[string]string{"dep": "the only thing this node produced"}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, vetting.Config{})

	if !strings.Contains(got, "the only thing this node produced") {
		t.Errorf("dependent prompt should keep the answer text when there is no artifact:\n%s", got)
	}
}

// TestBuildTask_KeepsAnswerWhenArtifactNotLonger: a short document must not
// replace a longer answer - the answer can legitimately be the fuller text.
func TestBuildTask_KeepsAnswerWhenArtifactNotLonger(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte("short"), "text/markdown", "doc:chat1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	plan := depPlan("document", "web-researcher")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	longAnswer := strings.Repeat("this answer is already the full deliverable. ", 10)
	upstream := map[string]string{"dep": longAnswer}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, nil, cfg)

	if !strings.Contains(got, longAnswer) {
		t.Errorf("dependent prompt should keep the longer answer text over a shorter artifact:\n%s", got)
	}
}

// TestBuildTask_ArtifactInliningKeepsGateFailedWarning proves the vetting
// warning still precedes the inlined content, not just the answer text.
func TestBuildTask_ArtifactInliningKeepsGateFailedWarning(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	full := strings.Repeat("unverified findings at length. ", 20)
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte(full), "text/markdown", "doc:chat1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	plan := depPlan("document", "web-researcher")
	cfg := vetting.Config{Artifacts: svc, User: "u1", ChatID: "chat1"}
	upstream := map[string]string{"dep": "short pointer"}
	gateFailed := map[string]bool{"dep": true}

	got := buildTask(context.Background(), plan, plan.Nodes[1], upstream, gateFailed, cfg)

	if !strings.Contains(got, "FAILED independent quality vetting") {
		t.Errorf("gate-failed warning missing once the dependency's artifact is inlined:\n%s", got)
	}
	if !strings.Contains(got, "unverified findings at length.") {
		t.Errorf("dependent prompt missing the inlined artifact alongside the warning:\n%s", got)
	}
}
