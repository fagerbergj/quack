package dag

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// steerAwareStub returns an empty worker draft normally (→ empty-recovery →
// ErrNodeEmpty → pause), but a real answer once the prompt carries the steer
// "Guidance" marker — so a steered re-run recovers the node.
type steerAwareStub struct{}

func (steerAwareStub) Name() string { return "steerAwareStub" }
func (steerAwareStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9}), nil)
			return
		}
		if strings.Contains(gUserText(req), "Guidance") {
			yield(gText("STEERED-ANSWER with a source [1](http://x)"), nil)
			return
		}
		yield(gText(""), nil)
	}
}

func resumeContent(interruptID string, payload any) *genai.Content {
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: interruptID, Name: "adk_request_input", Response: map[string]any{"payload": payload}},
	}}}
}

func runToPause(t *testing.T, r *runner.Runner, sess string) string {
	t.Helper()
	var id string
	for ev, err := range r.Run(context.Background(), "u", sess, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil && ev.RequestedInput != nil {
			id = ev.RequestedInput.InterruptID
		}
	}
	return id
}

func newHITLRunner(t *testing.T) *runner.Runner {
	t.Helper()
	stub := steerAwareStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}
	workerNode, err := vetting.NewWorkerNode(ag)
	if err != nil {
		t.Fatal(err)
	}
	gateNode := newGatedNode(plan, plan.Nodes[0], workerNode, nil, vetting.NewJudgeFactory(stub, nil), vetting.Config{Threshold: 0.6, JudgeRounds: 1}, nil, nil, "", true)
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			return workflow.RunNode[string](ctx, gateNode, plan.UserMessage)
		}, workflow.NodeConfig{})
	root, err := workflowagent.New(workflowagent.Config{Name: "t", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "t", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestExecute_EmptyNode_PausesThenSteerRecovers: an empty node pauses for input,
// and a steer re-runs it with guidance to a real answer.
func TestExecute_EmptyNode_PausesThenSteerRecovers(t *testing.T) {
	r := newHITLRunner(t)
	id := runToPause(t, r, "s1")
	if id == "" {
		t.Fatal("empty node did not pause for input")
	}
	var final string
	for ev, err := range r.Run(context.Background(), "u", "s1", resumeContent(id, "steer: focus on 2026 milestones"), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if ev != nil {
			if s, ok := ev.Output.(string); ok && strings.Contains(s, "STEERED-ANSWER") {
				final = s
			}
		}
	}
	if !strings.Contains(final, "STEERED-ANSWER") {
		t.Errorf("steer did not recover the node; final output = %q", final)
	}
}

// TestExecute_EmptyNode_CancelEmpties: cancelling a paused node yields empty
// output (continue-but-warn), without recovering.
func TestExecute_EmptyNode_CancelEmpties(t *testing.T) {
	r := newHITLRunner(t)
	id := runToPause(t, r, "s2")
	if id == "" {
		t.Fatal("empty node did not pause for input")
	}
	for ev, err := range r.Run(context.Background(), "u", "s2", resumeContent(id, "cancel"), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if ev != nil {
			if s, ok := ev.Output.(string); ok && strings.Contains(s, "STEERED-ANSWER") {
				t.Errorf("cancel should NOT recover; got %q", s)
			}
		}
	}
}

// TestExecute_EmptyNode_SurfacesNeedsInput: Execute emits node_needs_input for a
// paused (empty) node and does NOT emit a spurious node_done for it.
func TestExecute_EmptyNode_SurfacesNeedsInput(t *testing.T) {
	stub := steerAwareStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	var needsInput, spuriousDone bool
	var interruptID string
	events, _ := runPlanSSE(t, ex, plan, "chat", true)
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.NodeNeedsInputData:
			needsInput = true
			interruptID = d.InterruptID
			if d.NodeID != "n1" {
				t.Errorf("needs_input node_id = %q, want n1", d.NodeID)
			}
		case stream.NodeDoneData:
			if d.NodeID == "n1" {
				spuriousDone = true
			}
		}
	}
	if !needsInput {
		t.Error("no node_needs_input emitted for the empty node")
	}
	if interruptID == "" {
		t.Error("node_needs_input carried no interrupt_id")
	}
	if spuriousDone {
		t.Error("emitted a spurious node_done for the paused node")
	}
}

// TestExecute_EmptyNode_AutonomousFailsLoud: with interactive=false an empty node
// does NOT pause — it continue-but-warns AND surfaces as a loud node_failed (not a
// quiet node_done), so the gap is explicit.
func TestExecute_EmptyNode_AutonomousFailsLoud(t *testing.T) {
	stub := steerAwareStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	var failed, needsInput, done bool
	events, _ := runPlanSSE(t, ex, plan, "chat", false) // autonomous
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.NodeFailedData:
			if d.NodeID == "n1" {
				failed = true
			}
		case stream.NodeNeedsInputData:
			needsInput = true
		case stream.NodeDoneData:
			if d.NodeID == "n1" {
				done = true
			}
		}
	}
	if needsInput {
		t.Error("autonomous mode should NOT pause (node_needs_input)")
	}
	if !failed {
		t.Error("empty node should surface as node_failed")
	}
	if done {
		t.Error("empty node should NOT emit a quiet node_done")
	}
}
