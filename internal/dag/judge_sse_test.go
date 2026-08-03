package dag

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestExecute_JudgeStreamsAsStageJudge verifies the judge - which runs in an
// isolated runner OFF the workflow event stream - is surfaced to the SSE client
// as stage:judge agent runs (start + complete with the score), for every node,
// on the real Execute path. Run with -race: the fan-out nodes stream their judge
// concurrently through the executor's shared (mutex-guarded) yield sink.
func TestExecute_JudgeStreamsAsStageJudge(t *testing.T) {
	stub := stubG{}
	mk := func(name, role string) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: name, Instruction: role + " Answer."})
		if err != nil {
			t.Fatalf("agent %s: %v", name, err)
		}
		return a
	}
	agents := map[string]adkagent.Agent{
		"r1":    mk("r1", "ROLE:r1"),
		"r2":    mk("r2", "ROLE:r2"),
		"synth": mk("synth", "ROLE:synth"),
	}
	judge := vetting.NewJudgeFactory(stub, nil, nil)
	cfgFor := func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }
	ex := NewExecutor(session.InMemoryService(), agents, nil, nil, judge, cfgFor, nil)

	plan := Plan{ID: "t", UserMessage: "compare A and B", Nodes: []Node{
		{ID: "r1", AgentName: "r1", Task: "research A"},
		{ID: "r2", AgentName: "r2", Task: "research B"},
		{ID: "synth", AgentName: "synth", Task: "synthesize", DependsOn: []string{"r1", "r2"}},
	}}

	judgeStart := map[string]bool{}
	judgeDoneScored := map[string]bool{}
	events, _ := runPlanSSE(t, ex, plan, "chat")
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.AgentStartData:
			if d.Stage == stream.StageJudge {
				judgeStart[d.NodeID] = true
			}
		case stream.AgentCompleteData:
			if d.Stage == stream.StageJudge && d.Score > 0 {
				judgeDoneScored[d.NodeID] = true
			}
		}
	}

	for _, n := range []string{"r1", "r2", "synth"} {
		if !judgeStart[n] {
			t.Errorf("node %q: no stage:judge agent_start (judge run not surfaced to UI)", n)
		}
		if !judgeDoneScored[n] {
			t.Errorf("node %q: no stage:judge agent_complete with a score", n)
		}
	}
}
