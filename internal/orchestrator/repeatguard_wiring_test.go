package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/vetting"
)

// repeatLoopStub scripts the QA rig's live failure (a 9B model sent the same
// malformed create_plan call 386 times in a row - internal/tools' repeat
// guard was never wired to the orchestrator's own hand-built tools, unlike a
// worker node's tools.Build path, which always applied it). It keeps
// re-issuing the identical bad call until it sees the guard's own "REFUSED"
// text in the tool response, then stops - proving the guard now reaches
// create_plan through the orchestrator's tool list.
type repeatLoopStub struct{}

func (*repeatLoopStub) Name() string { return "repeatLoopStub" }

func (s *repeatLoopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if lastToolResponseContains(req, "REFUSED") {
			yield(stubText("Understood, stopping."), nil)
			return
		}
		yield(stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{"task": "x"}}}), nil)
	}
}

// lastToolResponseContains reports whether the most recent FunctionResponse
// in req's history contains substr.
func lastToolResponseContains(req *model.LLMRequest, substr string) bool {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil {
				continue
			}
			b, err := json.Marshal(p.FunctionResponse.Response)
			return err == nil && strings.Contains(string(b), substr)
		}
	}
	return false
}

// TestOrchestratorRepeatGuardStopsIdenticalCreatePlanLoop is the QA rig
// regression test: a model spamming the identical malformed create_plan
// call must be refused by the 3rd attempt (the same identical-call breaker
// tools.Build already applies to every worker node's own tools), not left
// to repeat until the context window itself runs out.
func TestOrchestratorRepeatGuardStopsIdenticalCreatePlanLoop(t *testing.T) {
	stub := &repeatLoopStub{}
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

	runTurn(t, o, "do the flaky retry loop review")

	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "stopping") {
		t.Fatalf("answer = %q, want the model's own recovery text once the repeat guard refused it - "+
			"if this never arrives, create_plan isn't actually guarded and the run would loop forever", answer)
	}
}

// repeatWriteArtifactStub is repeatLoopStub's twin for write_artifact - a
// hand-built tool appended AFTER the DAG tools (orchestrator.go), the class
// the repeat guard used to skip entirely.
type repeatWriteArtifactStub struct{}

func (*repeatWriteArtifactStub) Name() string { return "repeatWriteArtifactStub" }

func (s *repeatWriteArtifactStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if lastToolResponseContains(req, "REFUSED") {
			yield(stubText("Understood, stopping."), nil)
			return
		}
		yield(stubCall("write_artifact", map[string]any{"kind": "text", "mime": "text/plain", "bytes": "aGVsbG8="}), nil)
	}
}

// TestOrchestratorRepeatGuardCoversAppendedTools proves the guard wraps the
// WHOLE final tool list, not just the five DAG tools set up before memory/
// artifact tools are appended - a model spamming an identical write_artifact
// call must be refused too.
func TestOrchestratorRepeatGuardCoversAppendedTools(t *testing.T) {
	stub := &repeatWriteArtifactStub{}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, nil, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())

	runTurn(t, o, "write the same artifact over and over")

	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "stopping") {
		t.Fatalf("answer = %q, want the model's own recovery text once the repeat guard refused write_artifact - "+
			"if this never arrives, tools appended after the DAG five aren't guarded", answer)
	}
}
