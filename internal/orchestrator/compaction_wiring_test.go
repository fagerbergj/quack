package orchestrator

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/vetting"
)

// With SetCompaction and CompactionInterval 1, one direct-answer turn must record a compaction
// event on the long-lived chat session.
func TestOrchestratorRunnerCompactsTheChatSession(t *testing.T) {
	stub := &orchStub{replies: []*model.LLMResponse{stubText("hello there")}}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": worker},
		map[string]model.LLM{"web-researcher": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)

	compCfg, err := agent.NativeCompactionConfig(agent.Compaction{
		Summarizer: stub, ContextWindow: 1000, Enabled: true, CompactionInterval: 1,
	})
	if err != nil {
		t.Fatalf("NativeCompactionConfig: %v", err)
	}
	o.SetCompaction(compCfg)

	runTurn(t, o, "hi")

	resp, err := sessions.Get(context.Background(), &session.GetRequest{AppName: AppName, UserID: "u", SessionID: "chat"})
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	sawCompaction := false
	for ev := range resp.Session.Events().All() {
		if ev != nil && ev.Actions.Compaction != nil {
			sawCompaction = true
		}
	}
	if !sawCompaction {
		t.Fatal("no compaction event recorded on the chat session; SetCompaction did not reach the orchestrator's runner")
	}
}
