package orchestrator

import (
	"context"
	"errors"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/vetting"
)

// emptyWorkerModel always answers with an empty completion - the true
// silent-gap shape (#568), not an error.
type emptyWorkerModel struct{}

func (emptyWorkerModel) Name() string { return "empty-worker" }

func (emptyWorkerModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestRunBoundPlan_ClearsStalePlanningFailureSoALaterSilentGapStaysASilentGap
// is #1156's follow-up leak fix: a chat that once failed planning with a
// gateway error leaves a record under the orchestrator's own empty
// node/agent key (store.orchestratorGiveUpError's read target).
// RunBoundPlan makes no orchestrator model call of its own to ever naturally
// clear that record via RecordCallResult's success path (unlike Run, whose
// own planning call would) - without clearing it at entry, a LATER bound run
// that legitimately ends in its own silent gap would have
// store.DeriveTerminalStatus misreport it as failed, citing the OLD error.
func TestRunBoundPlan_ClearsStalePlanningFailureSoALaterSilentGapStaysASilentGap(t *testing.T) {
	const chatID = "chat-stale-1156"
	inference.RecordCallResult(chatID, "", "", errors.New(`status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`))
	t.Cleanup(func() { inference.ClearFailure(chatID, "", "") })

	stub := emptyWorkerModel{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": ag},
		map[string]model.LLM{"web-researcher": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)

	plan := dag.Plan{ID: "p1", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "web-researcher", Task: "research"}}}
	for range o.RunBoundPlan(context.Background(), "u", chatID, SourceApp, plan) {
	}

	if _, streak, _, ok := inference.LastFailure(chatID, "", ""); ok && streak > 0 {
		t.Fatalf("stale planning-failure record survived RunBoundPlan (streak=%d) - it leaks into a later unrelated silent gap", streak)
	}

	// The chat's next turn ends in a TRUE silent gap (no dag node failure
	// recorded at all) - the exact shape a later bound run's own empty
	// completion produces. With the stale record cleared, this must derive
	// idle/"", not resurrect the old 502 via orchestratorGiveUpError.
	turns := []store.TurnContent{{AsstText: ""}}
	status, _, nodeError := store.DeriveTerminalStatus(chatID, turns, "", false)
	if status != store.RunStatusIdle || nodeError != "" {
		t.Fatalf("DeriveTerminalStatus = %q/%q, want idle/\"\" - a later run's true silent gap must not resurrect the stale 502", status, nodeError)
	}
}
