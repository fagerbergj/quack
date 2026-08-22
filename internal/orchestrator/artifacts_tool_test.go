package orchestrator

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
)

// artifactToolStub records whether "load_artifacts" was among the tools
// offered on the orchestrator's turn, then answers with plain text so the
// run finishes without needing a real plan.
type artifactToolStub struct {
	mu   sync.Mutex
	seen bool
}

func (*artifactToolStub) Name() string { return "artifactToolStub" }

func (s *artifactToolStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		if stubHasTool(req, "load_artifacts") {
			s.seen = true
		}
		s.mu.Unlock()
		yield(stubText("ok"), nil)
	}
}

func (s *artifactToolStub) sawLoadArtifacts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// TestOrchestrator_LoadArtifactsTool_GatedOnSetArtifacts proves load_artifacts
// is offered to the model only once SetArtifacts has wired a backend - an
// unconfigured deployment (o.artifacts == nil) degrades to no tool, not a
// crash, and gets exactly the pre-existing tool set otherwise.
func TestOrchestrator_LoadArtifactsTool_GatedOnSetArtifacts(t *testing.T) {
	stub := &artifactToolStub{}
	o := newTestOrch(t, &orchStub{}) // real plan/execute plumbing, unused here
	o.model = stub
	runTurn(t, o, "hello")
	if stub.sawLoadArtifacts() {
		t.Fatal("load_artifacts offered with no artifact service configured")
	}

	stub2 := &artifactToolStub{}
	o2 := newTestOrch(t, &orchStub{})
	o2.model = stub2
	o2.SetArtifacts(artifact.InMemoryService())
	runTurn(t, o2, "hello")
	if !stub2.sawLoadArtifacts() {
		t.Fatal("load_artifacts not offered after SetArtifacts")
	}
}
