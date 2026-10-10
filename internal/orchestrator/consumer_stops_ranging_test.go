package orchestrator

import (
	"context"
	"fmt"
	"iter"
	"sync"
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

const fanN = 6

// slowWorkerStub holds each worker call open long enough that nodes are still
// mid-flight when the consumer walks away.
type slowWorkerStub struct {
	orchStub
	mu sync.Mutex
}

func (s *slowWorkerStub) GenerateContent(ctx context.Context, req *model.LLMRequest, st bool) iter.Seq2[*model.LLMResponse, error] {
	if !stubHasTool(req, "create_plan") {
		time.Sleep(10 * time.Millisecond)
	}
	return s.orchStub.GenerateContent(ctx, req, st)
}

// planFanout: fanN nodes plus a synthesizer depending on them by 0-based position (ids are minted).
func planFanout() *model.LLMResponse {
	var assignments, deps []any
	for i := range fanN {
		assignments = append(assignments, map[string]any{
			"agent": fmt.Sprintf("w%d", i), "task": "do it",
		})
		deps = append(deps, fmt.Sprintf("%d", i))
	}
	assignments = append(assignments, map[string]any{"agent": "synthesizer", "task": "synth", "depends_on": deps})
	return stubCall("create_plan", map[string]any{"assignments": assignments})
}

// The consumer stops ranging while sibling nodes still hold the ctx yield; the next node emit
// must not panic the process at the range site.
func TestRun_ConsumerStopsRangingMidRun_ProcessSurvives(t *testing.T) {
	stub := &slowWorkerStub{orchStub: orchStub{replies: []*model.LLMResponse{planFanout()}}}
	agents := map[string]adkagent.Agent{}
	models := map[string]model.LLM{}
	var infos []dag.AgentInfo
	for i := range fanN {
		name := fmt.Sprintf("w%d", i)
		a, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: "w", Instruction: "ROLE:w"})
		if err != nil {
			t.Fatalf("worker %s: %v", name, err)
		}
		agents[name], models[name] = a, stub
		infos = append(infos, dag.AgentInfo{Name: name, Description: "worker " + name})
	}
	sa, err := llmagent.New(llmagent.Config{Name: "synthesizer", Model: stub, Description: "s", Instruction: "ROLE:s"})
	if err != nil {
		t.Fatalf("synthesizer: %v", err)
	}
	agents["synthesizer"], models["synthesizer"] = sa, stub
	infos = append(infos, dag.AgentInfo{Name: "synthesizer", Description: "synthesizes"})

	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, nil, nil, vetting.NewJudgeFactory(stub, nil, nil), nil, nil)
	ex.SetRoster(&dag.Roster{Gen: 1, Agents: agents, Models: models, Infos: infos,
		CfgFor:  func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		SpecFor: func(string) dag.AdmissionSpec { return dag.AdmissionSpec{Model: "m"} }})
	ex.SetMaxActive(fanN)
	// Cap 1 so most nodes queue and fire onQueued through the ctx yield long
	// after the consumer has gone.
	admission := dag.NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	ex.SetAdmission(admission, dag.AdmissionSpec{})

	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, dag.NewPlanner(infos, nil, nil), ex, nil, nil, nil)

	var sawStart bool
	for ev, err := range o.Run(context.Background(), "u", "chat", SourceApp, "fan out", nil) {
		_ = err
		if ev.Name == stream.EventNodeStart {
			sawStart = true
			break // consumer stops ranging, as runChat does on an error event
		}
	}
	if !sawStart {
		t.Fatal("never reached a node_start - the early-exit path was not exercised")
	}
	// Nodes are still running; give them time to emit into the dead yield.
	time.Sleep(500 * time.Millisecond)
}
