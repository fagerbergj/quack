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

// TestOrchestratorRunnerCompactsTheChatSession is a regression test for the
// ADK audit's A3 finding: orchestrator.go's own runner.Config never set
// Compaction, so the long-lived chat session (the one that persists across every turn, unlike a worker node's ephemeral one) grew unbounded even with compaction enabled everywhere else. With SetCompaction wired in and CompactionInterval:1, a sliding-window pass must fire and record a compaction event on the session after just one turn that answers directly (no plan/execute round).
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
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)

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
