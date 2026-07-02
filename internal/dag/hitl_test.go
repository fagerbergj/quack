package dag

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

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
	root, err := BuildWorkflow(plan, map[string]adkagent.Agent{"w": ag}, nil, vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil, nil, "")
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
