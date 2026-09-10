package dag

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/vetting"
)

// advisorTaskSnoopStub records the AdvisorTask newGatedNode registered for
// the node's own round, the same seam advisorSnoopStub (readonly_wiring_test.go) uses.
type advisorTaskSnoopStub struct {
	mu   sync.Mutex
	saw  bool
	task vetting.AdvisorTask
}

func (s *advisorTaskSnoopStub) Name() string { return "advisorTaskSnoopStub" }

func (s *advisorTaskSnoopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if token, ok := vetting.ParseAdvisorThread(gUserText(req)); ok {
			if at, ok := vetting.LookupAdvisorThread(token); ok {
				s.mu.Lock()
				s.saw, s.task = true, at
				s.mu.Unlock()
			}
		}
		yield(gText("ANSWER with a source [1](http://x)"), nil)
	}
}

// TestNewGatedNode_ContinueSeedsACPSessionAndScope pins the two effects a
// granted continue: must have on the node it resumes into: the ACP protocol
// session id rides on the AdvisorTask so acp.round's session/load finds it,
// and the workspace scope is the PRIOR node's own (not this node's default),
// so ACPStateDir/clone resolution land in the same directory the prior
// session's state lives in.
func TestNewGatedNode_ContinueSeedsACPSessionAndScope(t *testing.T) {
	plan := Plan{ID: "t-continue", UserMessage: "x", Nodes: []Node{{ID: "n2", AgentName: "w", Continue: "n1"}}}
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 1, NodeID: "n2", ExternalWorker: true}
	resume := continuation{ok: true, handle: SessionHandle{Kind: "acp", ID: "prior-acp-session", Agent: "w", Scope: "n1"}, priorOutput: "the prior node's answer"}
	// buildGateNodes normally applies this override before newGatedNode is
	// built (see resolveContinue's call site) - mirrored here since this
	// test drives newGatedNode directly.
	cfg.NodeID = resume.handle.Scope

	stub := &advisorTaskSnoopStub{}
	runSingleNodeResumed(t, plan, cfg, stub, nil, resume)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.saw {
		t.Fatal("the worker round never found a registered AdvisorTask for its own node")
	}
	if stub.task.ACPSessionID != "prior-acp-session" {
		t.Errorf("AdvisorTask.ACPSessionID = %q, want the prior node's session id", stub.task.ACPSessionID)
	}
	if stub.task.WorkspaceNodeID != "n1" {
		t.Errorf("AdvisorTask.WorkspaceNodeID = %q, want the prior node's own scope %q", stub.task.WorkspaceNodeID, "n1")
	}
}

// TestNewGatedNode_NoContinueLeavesACPSessionIDEmpty is the contrast case: a
// fresh node (no granted continuation) must never carry a stale session id.
func TestNewGatedNode_NoContinueLeavesACPSessionIDEmpty(t *testing.T) {
	plan := Plan{ID: "t-fresh", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w"}}}
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 1, NodeID: "n1", ExternalWorker: true}

	stub := &advisorTaskSnoopStub{}
	runSingleNode(t, plan, cfg, stub, nil)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.saw {
		t.Fatal("the worker round never found a registered AdvisorTask for its own node")
	}
	if stub.task.ACPSessionID != "" {
		t.Errorf("AdvisorTask.ACPSessionID = %q, want empty for a fresh node", stub.task.ACPSessionID)
	}
}
