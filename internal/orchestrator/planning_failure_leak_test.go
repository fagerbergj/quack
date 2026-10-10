package orchestrator

import (
	"context"
	"errors"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/vetting"
)

// A stale gateway failure under the orchestrator's empty key must not make a later bound run's own
// silent gap misreport as failed: RunBoundPlan makes no model call that would clear it.
func TestRunBoundPlan_ClearsStalePlanningFailureSoALaterSilentGapStaysASilentGap(t *testing.T) {
	const chatID = "chat-stale-1156"
	inference.RecordCallResult(chatID, "", "", errors.New(`status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`))
	t.Cleanup(func() { inference.ClearFailure(chatID, "", "") })

	stub := replyModel("")
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
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)

	plan := dag.Plan{ID: "p1", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "web-researcher", Task: "research"}}}
	for range o.RunBoundPlan(context.Background(), "u", chatID, SourceApp, plan) {
	}

	if _, streak, _, ok := inference.LastFailure(chatID, "", ""); ok && streak > 0 {
		t.Fatalf("stale planning-failure record survived RunBoundPlan (streak=%d) - it leaks into a later unrelated silent gap", streak)
	}

	// A true silent gap must derive idle, not resurrect the old 502 via orchestratorGiveUpError.
	turns := []store.TurnContent{{AsstText: ""}}
	status, _, nodeError := store.DeriveTerminalStatus(chatID, turns, "", false)
	if status != store.RunStatusIdle || nodeError != "" {
		t.Fatalf("DeriveTerminalStatus = %q/%q, want idle/\"\" - a later run's true silent gap must not resurrect the stale 502", status, nodeError)
	}
}
