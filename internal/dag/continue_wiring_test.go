package dag

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

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

// TestNewGatedNode_CapturesADKBranchOnFinish runs node n1 for real and
// checks buildSessionHandle captures its REAL (ADK-scheduler-assigned)
// branch - the durable coordinate a later turn's continue: reuses (see
// resumeADKContext/emitPriorAnswer).
func TestNewGatedNode_CapturesADKBranchOnFinish(t *testing.T) {
	sessions := session.InMemoryService()
	const userID, sessID = "u", "s"

	var captured SessionHandle
	recordSession := func(nodeID string, h SessionHandle) { captured = h }
	plan1 := Plan{ID: "t1", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w"}}}
	cfg1 := vetting.Config{NodeID: "n1"}
	stub1 := okStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub1, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	wn, err := vetting.NewWorkerNode(ag)
	if err != nil {
		t.Fatal(err)
	}
	gateNodes := map[string]workflow.Node{
		"n1": newGatedNode(plan1, plan1.Nodes[0], wn, nil, ag, nil, nil, cfg1, nil, nil, "", nil, nil, nil, AdmissionSpec{}, nil, nil, continuation{}, recordSession),
	}
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			_, err := runDAGSubset(ctx, plan1, gateNodes, 1, nil, map[string]bool{"n1": true})
			return "done", err
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: sessions, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(context.Background(), userID, sessID, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if captured.Kind != "adk" || captured.Branch == "" {
		t.Fatalf("n1's captured handle = %+v, want a non-empty adk-kind Branch", captured)
	}
}

// captureEmit records every event handed to it, standing in for the
// dynamic-node emit callback without a real ADK run.
type captureEmit struct {
	mu     sync.Mutex
	events []*session.Event
}

func (c *captureEmit) fn() func(*session.Event) error {
	return func(ev *session.Event) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.events = append(c.events, ev)
		return nil
	}
}

// TestEmitPriorAnswer_TagsEventForTheResumedBranch pins what emitPriorAnswer
// controls directly: the injected event carries the resumed branch/isolation
// scope and the prior answer as a "model"-role turn authored by the SAME
// agent (so ADK's own foreign-event conversion leaves it alone - see
// ConvertForeignEvent) - the coordinates a continuing node's own round
// (runWorkerNode's inheritIsolation) is scoped to match.
//
// Whether ADK's workflow system actually feeds this event back into a
// wrapped LlmAgent's own request is a SEPARATE question this test does not
// answer: workflow.NewAgentNode forces ModeSingleTurn on every node it
// wraps, and empirically (four isolated diagnostics against this same ADK
// version, not committed here) that drops ALL prior-invocation history from
// ContentsRequestProcessor's output regardless of branch/isolation scope -
// a plain native model.LLM worker wrapped via vetting.NewWorkerNode would
// need that ADK-level default addressed separately to see this event at
// all; production's ACP/A2A-backed workers are never llminternal.Agent, so
// the forcing doesn't apply to them, and they build their outbound message
// from session events directly (see emitPrompt's doc comment) rather than
// through ContentsRequestProcessor.
func TestEmitPriorAnswer_TagsEventForTheResumedBranch(t *testing.T) {
	c := &captureEmit{}
	resume := continuation{ok: true, handle: SessionHandle{Kind: "adk", Branch: "n1@1", IsolationScope: "scope-1"}, priorOutput: "PRIOR_ANSWER_MARKER"}
	emitPriorAnswer(&adkagent.ContextMock{}, c.fn(), "w", resume)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(c.events))
	}
	ev := c.events[0]
	if ev.Branch != "n1@1" || ev.IsolationScope != "scope-1" || ev.Author != "w" {
		t.Fatalf("event = %+v, want Branch=n1@1 IsolationScope=scope-1 Author=w", ev)
	}
	if ev.LLMResponse.Content == nil || ev.LLMResponse.Content.Role != "model" || gContentText(ev.LLMResponse.Content) != "PRIOR_ANSWER_MARKER" {
		t.Fatalf("event content = %+v, want a model-role turn carrying the prior answer", ev.LLMResponse.Content)
	}
}

// TestEmitPriorAnswer_NoopCases pins when emitPriorAnswer must NOT touch the
// session at all - a fresh node, an ACP continuation (session/load carries
// it instead), or a legacy handle with no captured branch.
func TestEmitPriorAnswer_NoopCases(t *testing.T) {
	cases := []struct {
		name string
		c    continuation
	}{
		{"not ok", continuation{}},
		{"acp kind", continuation{ok: true, handle: SessionHandle{Kind: "acp", Branch: "n1"}, priorOutput: "x"}},
		{"no captured branch", continuation{ok: true, handle: SessionHandle{Kind: "adk"}, priorOutput: "x"}},
		{"no prior output", continuation{ok: true, handle: SessionHandle{Kind: "adk", Branch: "n1@1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &captureEmit{}
			emitPriorAnswer(&adkagent.ContextMock{}, c.fn(), "w", tc.c)
			if len(c.events) != 0 {
				t.Fatalf("%s: emitted %d events, want 0 (no-op)", tc.name, len(c.events))
			}
		})
	}
}
