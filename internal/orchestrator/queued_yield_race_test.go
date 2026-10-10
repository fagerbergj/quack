package orchestrator

import (
	"context"
	"iter"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// slowOrchStub delays worker/judge replies so a queued node's onQueued overlaps its siblings.
type slowOrchStub struct{ orchStub }

func (s *slowOrchStub) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if !stubHasTool(req, "create_plan") {
		time.Sleep(15 * time.Millisecond)
	}
	return s.orchStub.GenerateContent(ctx, req, stream)
}

// planCallQueueing: 3 ready researchers against an admission cap of 2, so one must queue.
// depends_on uses 0-based positions since node ids are minted; delivery makes the plan complete.
func planCallQueueing() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{
		"assignments": []any{
			map[string]any{"agent": "web-researcher", "task": "research qualities"},
			map[string]any{"agent": "web-researcher", "task": "research when"},
			map[string]any{"agent": "web-researcher", "task": "research conventions"},
			map[string]any{"agent": "synthesizer", "task": "synthesize", "depends_on": []any{"0", "1", "2"}},
		},
		"delivery": map[string]any{"kind": "comment"},
	})
}

// onQueued yields from the queuing node's goroutine concurrently with siblings' events; under
// -race, the plain slice below proves every write goes through newSafeYield's mutex.
func TestRun_NodeQueuedDuringSiblingRun_NoUnsynchronizedYield(t *testing.T) {
	stub := &slowOrchStub{orchStub{replies: []*model.LLMResponse{planCallQueueing()}}}

	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("web-researcher agent: %v", err)
	}
	synth, err := llmagent.New(llmagent.Config{
		Name: "synthesizer", Model: stub, Description: "synthesizer", Instruction: "ROLE:synth",
	})
	if err != nil {
		t.Fatalf("synthesizer agent: %v", err)
	}

	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, nil, nil, vetting.NewJudgeFactory(stub, nil, nil), nil, nil)
	ex.SetRoster(&dag.Roster{
		Gen: 1,
		Infos: []dag.AgentInfo{
			{Name: "web-researcher", Description: "researches the web"},
			{Name: "synthesizer", Description: "synthesizes findings"},
		},
		Agents: map[string]adkagent.Agent{"web-researcher": worker, "synthesizer": synth},
		Models: map[string]model.LLM{"web-researcher": stub, "synthesizer": stub},
		CfgFor: func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		SpecFor: func(agentName string) dag.AdmissionSpec {
			if agentName == "web-researcher" {
				return dag.AdmissionSpec{Model: "wr"}
			}
			return dag.AdmissionSpec{}
		},
	})

	// Cap web-researcher at 1 concurrent session: 3 ready nodes, so 2 MUST
	// queue and fire onQueued while a sibling is still executing.
	admission := dag.NewAdmission(map[string]int{"wr": 1}, nil, nil, 0)
	ex.SetAdmission(admission, dag.AdmissionSpec{})

	planner := dag.NewPlanner(ex.RosterFor(context.Background()).Infos, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)

	// Unsynchronized append - the shape production and runTurn both use.
	// Intentional: this is the artifact that catches the race under -race.
	var evs []stream.SSEEvent
	var sawQueued bool
	for ev, err := range o.Run(context.Background(), "u", "chat", SourceApp, "compare qualities, when, and conventions", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		evs = append(evs, ev)
		if ev.Name == stream.EventNodeQueued {
			sawQueued = true
		}
	}

	// Non-vacuity: prove the queuing path actually fired, not just that the
	// run completed without ever exercising admission contention.
	if !sawQueued {
		t.Fatalf("no node_queued event observed - the test never exercised admission queueing; events=%v", evs)
	}
	if hasEvent(evs, stream.EventError) {
		t.Errorf("run surfaced an error event; events=%v", evs)
	}
}
