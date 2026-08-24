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

// slowOrchStub is orchStub with a few ms added to every worker/judge reply -
// widens the window admitted siblings stay "in flight" so a queued node's
// onQueued reliably overlaps them, instead of racing to finish first.
type slowOrchStub struct{ orchStub }

func (s *slowOrchStub) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if !stubHasTool(req, "plan") {
		time.Sleep(15 * time.Millisecond)
	}
	return s.orchStub.GenerateContent(ctx, req, stream)
}

// planCallQueueing authors 3 web-researcher nodes (no deps) fanning into a
// synthesizer - the live incident's shape (chat 65974150-3efd-439a-8186-
// b4a93ad59d7a): 3 ready nodes against an admission cap of 2, so one MUST
// queue while its siblings are still running.
func planCallQueueing() *model.LLMResponse {
	return stubCall("plan", map[string]any{"nodes": []any{
		map[string]any{"id": "qualities", "agent": "web-researcher", "task": "research qualities", "depends_on": []any{}},
		map[string]any{"id": "when", "agent": "web-researcher", "task": "research when", "depends_on": []any{}},
		map[string]any{"id": "conventions", "agent": "web-researcher", "task": "research conventions", "depends_on": []any{}},
		map[string]any{"id": "synth", "agent": "synthesizer", "task": "synthesize", "depends_on": []any{"qualities", "when", "conventions"}},
	}})
}

// TestRun_NodeQueuedDuringSiblingRun_NoUnsynchronizedYield reproduces #1021/
// #1027's remaining bug: onQueued (dag/graph.go) grabs the ctx-stored yield
// directly and calls it from the queuing node's own goroutine, bypassing
// newSafeYield's mutex. Orchestrator.Run wires that ctx BEFORE safeYield
// exists and with the unwrapped yield (every other entrypoint - RunBoundPlan,
// startNodeRun, RetryNode - wires it correctly). With 3 ready web-researcher
// nodes against a 2-session admission cap, the 3rd node's onQueued fires
// concurrently with the admitted siblings' own SSE events reaching this same
// consumer, from a different goroutine, with no synchronization between them.
//
// The consumer below appends to a plain, unguarded slice - exactly what
// production's REST handler and this package's own runTurn helper already do
// (internal/server/rest/handler.go's `res.Step`/`publish`, and
// internal/orchestrator/continue_test.go's runTurn). Run this under -race:
// present bug => data race; fixed => none, because every write funnels
// through newSafeYield's mutex.
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
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": worker, "synthesizer": synth},
		map[string]model.LLM{"web-researcher": stub, "synthesizer": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	// Cap web-researcher at 1 concurrent session: 3 ready nodes, so 2 MUST
	// queue and fire onQueued while a sibling is still executing.
	admission := dag.NewAdmission(map[string]int{"wr": 1}, nil, nil, 0)
	ex.SetAdmission(admission, func(agentName string) dag.AdmissionSpec {
		if agentName == "web-researcher" {
			return dag.AdmissionSpec{Model: "wr"}
		}
		return dag.AdmissionSpec{}
	})

	planner := dag.NewPlanner([]dag.AgentInfo{
		{Name: "web-researcher", Description: "researches the web"},
		{Name: "synthesizer", Description: "synthesizes findings"},
	}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)

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
