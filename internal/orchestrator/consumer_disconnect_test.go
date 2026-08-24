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
	if !stubHasTool(req, "plan") {
		time.Sleep(10 * time.Millisecond)
	}
	return s.orchStub.GenerateContent(ctx, req, st)
}

func planFanout() *model.LLMResponse {
	var nodes, deps []any
	for i := range fanN {
		nodes = append(nodes, map[string]any{
			"id": fmt.Sprintf("n%d", i), "agent": fmt.Sprintf("w%d", i),
			"task": "do it", "depends_on": []any{},
		})
		deps = append(deps, fmt.Sprintf("n%d", i))
	}
	nodes = append(nodes, map[string]any{"id": "synth", "agent": "synthesizer", "task": "synth", "depends_on": deps})
	return stubCall("plan", map[string]any{"nodes": nodes})
}

// #1033: an SSE client dropping mid-run makes the consumer break out of the
// range, so yield returns false while node goroutines still hold the ctx-stored
// yield. Pre-fix, the next node emit tripped "range function continued
// iteration after function for loop body returned false", safeYield recovered
// it without resuming, and Go killed the process at the range site. The run
// must abandon quietly instead.
func TestRun_ConsumerDisconnectsMidRun_ProcessSurvives(t *testing.T) {
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
	ex := dag.NewExecutor(sessions, agents, models, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetMaxActive(fanN)
	// Cap 1 so most nodes queue and fire onQueued through the ctx yield long
	// after the consumer has gone.
	admission := dag.NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	ex.SetAdmission(admission, func(string) dag.AdmissionSpec { return dag.AdmissionSpec{Model: "m"} })

	o := New(sessions, stub, "You are the orchestrator.", dag.NewPlanner(infos, nil, nil), ex, nil, nil, nil)

	var sawStart bool
	for ev, err := range o.Run(context.Background(), "u", "chat", SourceApp, "fan out", nil) {
		_ = err
		if ev.Name == stream.EventNodeStart {
			sawStart = true
			break // client disconnected
		}
	}
	if !sawStart {
		t.Fatal("never reached a node_start - the disconnect path was not exercised")
	}
	// Nodes are still running; give them time to emit into the dead yield.
	time.Sleep(500 * time.Millisecond)
}
