package dag

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestExecute_JudgeStreamsAsStageJudge verifies the judge — which runs in an
// isolated runner OFF the workflow event stream — is surfaced to the SSE client
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
	judge := vetting.NewJudgeFactory(stub, nil)
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

// TestExecute_AdvisorStreamsAsStageAdvisor verifies the formative advisor is
// consulted before each worker draft and surfaced to the UI as a stage:advisor
// run (translated from the advisor-rN RunNode child by dagStream).
func TestExecute_AdvisorStreamsAsStageAdvisor(t *testing.T) {
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
		"synth": mk("synth", "ROLE:synth"),
	}
	advisor := mk("advisor", "ROLE:advisor") // stub returns generic text; wiring is what we assert
	judge := vetting.NewJudgeFactory(stub, nil)
	cfgFor := func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }
	ex := NewExecutor(session.InMemoryService(), agents, nil, advisor, judge, cfgFor, nil)

	plan := Plan{ID: "t", UserMessage: "compare", Nodes: []Node{
		{ID: "r1", AgentName: "r1", Task: "research"},
		{ID: "synth", AgentName: "synth", Task: "synthesize", DependsOn: []string{"r1"}},
	}}

	advisorStart := map[string]bool{}
	events, _ := runPlanSSE(t, ex, plan, "chat")
	for _, ev := range events {
		if d, ok := ev.Data.(stream.AgentStartData); ok && d.Stage == stream.StageAdvisor {
			advisorStart[d.NodeID] = true
		}
	}

	for _, n := range []string{"r1", "synth"} {
		if !advisorStart[n] {
			t.Errorf("node %q: no stage:advisor run (advisor not consulted or not streamed)", n)
		}
	}
}

// advisorMemoryStub distinguishes its three roles (worker/judge/advisor) by
// system instruction and tool presence, so ONE model can play all three (as the
// other tests here do). It fails the judge once (forcing a revision) so the
// advisor is consulted twice — round 0 (the draft) and round 1 (the revision) —
// and captures whether round 1's advisor prompt saw round 0's own advice.
type advisorMemoryStub struct {
	mu                     sync.Mutex
	revisionSawPriorAdvice bool
	revisionPrompt         string
	judged                 int
}

func (*advisorMemoryStub) Name() string { return "advisorMemoryStub" }

func (s *advisorMemoryStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judged++
			n := s.judged
			s.mu.Unlock()
			if n == 1 {
				yield(gCall("submit_verdict", map[string]any{"score": 0.3, "feedback": "needs work"}), nil)
			} else {
				yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			}
			return
		}
		if strings.Contains(gSysText(req), "ROLE:advisor") {
			txt := gUserText(req)
			if strings.Contains(txt, "You advised on an earlier attempt") {
				s.mu.Lock()
				s.revisionSawPriorAdvice = strings.Contains(txt, "ADVICE-ROUND-0")
				s.revisionPrompt = txt
				s.mu.Unlock()
				yield(gText("ADVICE-ROUND-1"), nil)
				return
			}
			yield(gText("ADVICE-ROUND-0"), nil)
			return
		}
		yield(gText("draft"), nil)
	}
}

// TestAdvisor_RevisionConsultSeesItsOwnPriorAdvice: the advisor's revision-round
// consult must see what it ALREADY advised (round 0), not a cold restart — the
// gate carries lastAdvice forward across rounds since ADK's own session/branch
// mechanism can't (AgentNode forces single-turn mode, discarding history
// regardless of branch — see RunGatedRefine's consult closure).
func TestAdvisor_RevisionConsultSeesItsOwnPriorAdvice(t *testing.T) {
	stub := &advisorMemoryStub{}
	mk := func(name, role string) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: name, Instruction: role + " Answer."})
		if err != nil {
			t.Fatalf("agent %s: %v", name, err)
		}
		return a
	}
	worker := mk("blk", "ROLE:blk")
	advisor := mk("advisor", "ROLE:advisor")
	judge := vetting.NewJudgeFactory(stub, nil)
	cfgFor := func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": worker}, nil, advisor, judge, cfgFor, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	_, _ = runPlanSSE(t, ex, plan, "chat")

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.revisionSawPriorAdvice {
		t.Errorf("revision advisor consult did not see round 0's advice; prompt:\n%s", stub.revisionPrompt)
	}
}
