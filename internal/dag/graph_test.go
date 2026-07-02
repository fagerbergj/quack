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

// stubG is a deterministic model.LLM routing by agent role (system-instruction
// marker) and by the presence of the judge's submit_verdict tool. Researchers
// return distinct findings; the judge always passes; the synthesizer echoes the
// prompt it received — so a passing test proves both researcher outputs reached
// the synthesizer via JoinNode fan-in + buildTask assembly keyed by node ID.
type stubG struct{}

func (stubG) Name() string { return "stubG" }

func (stubG) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		sys := gSysText(req)
		switch {
		case strings.Contains(sys, "ROLE:r1"):
			yield(gText("ALPHA-FINDING"), nil)
		case strings.Contains(sys, "ROLE:r2"):
			yield(gText("BETA-FINDING"), nil)
		case strings.Contains(sys, "ROLE:synth"):
			yield(gText("SYNTH{"+gUserText(req)+"}"), nil)
		default:
			yield(gText("?"), nil)
		}
	}
}

// TestBuildWorkflow_FanInAssembly builds a two-researcher → synthesizer plan and
// runs it on the real ADK v2 engine, asserting the synthesizer's assembled prompt
// carried BOTH researcher outputs (fan-out → JoinNode fan-in → buildTask).
func TestBuildWorkflow_FanInAssembly(t *testing.T) {
	stub := stubG{}
	mk := func(name, role string) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{
			Name: name, Model: stub, Description: name,
			Instruction: role + " Answer the task.",
		})
		if err != nil {
			t.Fatalf("agent %s: %v", name, err)
		}
		return a
	}
	agents := map[string]adkagent.Agent{
		"researcher1": mk("researcher1", "ROLE:r1"),
		"researcher2": mk("researcher2", "ROLE:r2"),
		"synthesizer": mk("synthesizer", "ROLE:synth"),
	}
	plan := Plan{
		ID:          "p1",
		UserMessage: "compare alpha and beta",
		Nodes: []Node{
			{ID: "r1", AgentName: "researcher1", Task: "find alpha"},
			{ID: "r2", AgentName: "researcher2", Task: "find beta"},
			{ID: "synth", AgentName: "synthesizer", Task: "combine findings", DependsOn: []string{"r1", "r2"}},
		},
	}
	cfg := vetting.Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-10"}

	root, err := BuildWorkflow(plan, agents, vetting.NewJudgeFactory(stub, nil), cfg)
	if err != nil {
		t.Fatalf("BuildWorkflow: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	msg := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	var final string
	for ev, err := range r.Run(t.Context(), "u", "s", msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
		if s, ok := ev.Output.(string); ok && strings.HasPrefix(s, "SYNTH{") {
			final = s
		}
	}

	if final == "" {
		t.Fatal("no synthesizer output captured")
	}
	if !strings.Contains(final, "ALPHA-FINDING") || !strings.Contains(final, "BETA-FINDING") {
		t.Fatalf("synthesizer prompt missing a fan-in input; got %q", final)
	}
}

// --- stub helpers ---

func gHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func gSysText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	return gContentText(req.Config.SystemInstruction)
}

func gUserText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		b.WriteString(gContentText(c))
		b.WriteByte('\n')
	}
	return b.String()
}

func gContentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && !p.Thought && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func gText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func gCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}
