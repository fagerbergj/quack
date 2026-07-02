package dag

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestPrototype_SingleRunnerNativeHITL: the ADK-native shape. One orchestration
// workflow whose dynamic node runs the DAG (built by the EXISTING BuildWorkflow)
// via RunNode — all in ONE runner. The decisive question: does a gate node's
// empty-pause (RequestInput) propagate up through the nested DAG to the top
// runner, and does resume re-enter? If yes, the current gate/DAG code carries
// over and only the runner glue changes.
func TestPrototype_SingleRunnerNativeHITL(t *testing.T) {
	stub := steerAwareStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	// EXISTING code, unchanged: build the DAG workflow + gate.
	dagAgent, err := BuildWorkflow(plan, map[string]adkagent.Agent{"w": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// NEW shell: an orchestration workflow whose node runs the DAG via RunNode.
	dagNode, err := workflow.NewAgentNode(dagAgent, workflow.NodeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	orchestrate := workflow.NewDynamicNode[any, string]("orchestrate",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			// (a real orchestrator would make the planning LLM call here first)
			return workflow.RunNode[string](ctx, dagNode, plan.UserMessage)
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "orch", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	// Run to pause — ONE runner.
	var id string
	for ev, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil && ev.RequestedInput != nil {
			id = ev.RequestedInput.InterruptID
		}
	}
	if id == "" {
		t.Fatal("nested DAG pause did NOT propagate to the single top runner — native single-runner HITL does not work as-is")
	}
	t.Logf("PAUSED at top runner with id=%q — native single-runner HITL propagates through RunNode", id)

	// Resume — re-enter the ONE runner with the reply.
	var final string
	for ev, err := range r.Run(context.Background(), "u", "s", resumeContent(id, "steer: focus on 2026"), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if ev != nil {
			if s, ok := ev.Output.(string); ok && strings.Contains(s, "STEERED-ANSWER") {
				final = s
			}
		}
	}
	// SPIKE FINDING: the nested resume RESPONSE does not reach the inner gate node
	// via a wrapped sub-workflow (needs ResumeOrRequestInput and/or scheduling the
	// DAG nodes with direct RunNode rather than an AgentNode-wrapped sub-workflow).
	if strings.Contains(final, "STEERED-ANSWER") {
		t.Logf("RESUMED + recovered: %q", final)
	} else {
		t.Logf("resume gap: nested sub-workflow did not deliver the reply (final=%q) — see spike notes", final)
	}
}
