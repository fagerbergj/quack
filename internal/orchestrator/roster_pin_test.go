package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

func drain(seq func(func(stream.SSEEvent, error) bool)) {
	for range seq {
	}
}

// Every entrypoint's pin must be released on success, error and early-stop paths,
// or a retired roster (and its MCP processes) never dies.
func TestOrchestratorPinsAreBalanced(t *testing.T) {
	o := newTestOrch(t, &orchStub{replies: []*model.LLMResponse{stubText("ANSWER")}})
	boot := o.executor.RosterFor(context.Background())
	var deaths atomic.Int32
	r := &dag.Roster{Gen: 1, Agents: boot.Agents, Models: boot.Models, CfgFor: boot.CfgFor, OnDead: func() { deaths.Add(1) }}
	o.executor.SetRoster(r)
	bg := context.Background()

	drain(o.Run(bg, "u", "chat", SourceApp, "hello", nil))
	for range o.Run(bg, "u", "chat", SourceApp, "stop early", nil) {
		break
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	drain(o.Run(cancelled, "u", "chat2", SourceApp, "cancelled", nil))
	drain(o.RetryNode(bg, "u", "no-plan", nil, "n1", ""))
	drain(o.RunBoundPlan(bg, "u", "bound", SourceApp, dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "n", AgentName: "ghost", Task: "t"}}}))
	o.StartNode(bg, "u", "no-plan", "n1", "", func(stream.SSEEvent, error) bool { return true })
	if _, err := o.BuildBoundPlan(bg, []dag.RawNode{{ID: "n", Agent: "ghost", Task: "t"}}, "m", nil, nil); err == nil {
		t.Fatal("BuildBoundPlan accepted an unknown agent")
	}

	if deaths.Load() != 0 {
		t.Fatal("current roster died")
	}
	o.executor.SetRoster(&dag.Roster{Gen: 2})
	if deaths.Load() != 1 {
		t.Fatalf("retired roster OnDead = %d after all runs ended, want 1 (a pin leaked)", deaths.Load())
	}
}
