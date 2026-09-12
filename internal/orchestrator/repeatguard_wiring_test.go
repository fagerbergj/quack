package orchestrator

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// repeatLoopStub scripts the QA rig's live failure (a 9B model sent the same
// malformed create_plan call 386 times in a row - internal/tools' repeat
// guard was never wired to the orchestrator's own hand-built tools, unlike a
// worker node's tools.Build path, which always applied it). Unlike a model
// that reads the REFUSED error, this one IGNORES it and keeps re-issuing
// the identical bad call forever - proving the guard's own hard-stop tier
// (not the model choosing to stop) is what ultimately bounds it.
type repeatLoopStub struct{ calls int }

func (s *repeatLoopStub) Name() string { return "repeatLoopStub" }

func (s *repeatLoopStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.calls++
		yield(stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{"task": "x"}}}), nil)
	}
}

// TestOrchestratorRepeatGuardStopsIdenticalCreatePlanLoop is the QA rig
// regression test: a model spamming the identical malformed create_plan
// call, ignoring every REFUSED error, must still terminate - via the
// guard's own hard-stop tier once it keeps treading on regardless, in
// exactly ONE orchestrator invocation. A hard-stopped turn used to be
// retried unchanged (up to maxOrchestratorContinues times), reproducing the
// identical loop every time; it must give up on the first hard stop
// instead. The owner's settled direction (#slice3 review) is a SOFT refusal
// alone must never end the turn (that denies the model any chance to
// self-correct within it); this pins the other half - a model that ignores
// the soft refusal anyway is still bounded, to one invocation.
func TestOrchestratorRepeatGuardStopsIdenticalCreatePlanLoop(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

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

	evs := runTurn(t, o, "do the flaky retry loop review")

	// tools.repeatThreshold+repeatHardStopAfter+1 (3+2+1, unexported -
	// mirrored here as a literal): the hard-stop tier fires on the call
	// AFTER exceeding that sum, so one orchestrator invocation makes exactly
	// this many stub calls before ending itself.
	const guardAttemptsPerInvocation = 6
	if stub.calls != guardAttemptsPerInvocation {
		t.Fatalf("stub called %d times, want exactly %d - a hard-stopped turn must not be retried unchanged",
			stub.calls, guardAttemptsPerInvocation)
	}
	if !hasEvent(evs, stream.EventError) {
		t.Fatalf("want an error event once the orchestrator gives up on the malformed call, events=%v", evs)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if strings.Contains(answer, "assignments") {
		t.Fatalf("answer = %q, should not reflect a successful plan - the call never stopped being malformed", answer)
	}
	if strings.Contains(logs.String(), "continuing it") {
		t.Errorf("a hard-stopped turn must not be retried; logs=%q", logs.String())
	}
	if !strings.Contains(logs.String(), `attempts=1`) {
		t.Errorf("give-up log must report the honest attempt count (1, no retries); logs=%q", logs.String())
	}
}

// repeatWriteArtifactStub is repeatLoopStub's twin for write_artifact - a
// hand-built tool appended AFTER the DAG tools (orchestrator.go), the class
// the repeat guard used to skip entirely. Unlike repeatLoopStub, this one
// DOES correct itself once refused (different bytes) - the owner's settled
// direction pins that a soft refusal must let a model that corrects proceed
// within the SAME turn, not force it through a fresh wrapper-level retry.
type repeatWriteArtifactStub struct{ calls int }

func (s *repeatWriteArtifactStub) Name() string { return "repeatWriteArtifactStub" }

func (s *repeatWriteArtifactStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.calls++
		switch {
		case s.calls <= 3:
			// Identical spam - the 3rd is soft-refused (tools appended after
			// the DAG five must be guarded too).
			yield(stubCall("write_artifact", map[string]any{"kind": "text", "mime": "text/plain", "bytes": "aGVsbG8="}), nil)
		case s.calls == 4:
			// Corrects after the refusal (different bytes) - must be allowed
			// to run, proving the turn stayed open past the soft refusal.
			yield(stubCall("write_artifact", map[string]any{"kind": "text", "mime": "text/plain", "bytes": "Y29ycmVjdGVk"}), nil)
		default:
			yield(stubText("Understood, delivering the corrected write."), nil)
		}
	}
}

// TestOrchestratorRepeatGuardCoversAppendedTools proves the guard wraps the
// WHOLE final tool list, not just the five DAG tools set up before memory/
// artifact tools are appended (a model spamming an identical write_artifact
// call must be refused too), AND that a model correcting its call right
// after the refusal succeeds within the SAME turn - exactly 5 stub
// invocations (3 identical + 1 corrected + the final text), never needing a
// wrapper-level retry (#slice3 review: ending the turn on the first
// refusal used to deny exactly this recovery).
func TestOrchestratorRepeatGuardCoversAppendedTools(t *testing.T) {
	stub := &repeatWriteArtifactStub{}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, nil, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())

	runTurn(t, o, "write the same artifact, then correct it")

	if stub.calls != 5 {
		t.Fatalf("stub called %d times; want exactly 5 (3 identical + 1 corrected + final text) - "+
			"more means the turn ended too early and needed a wrapper-level retry to recover", stub.calls)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "delivering") {
		t.Fatalf("answer = %q, want the model's own text after its corrected write_artifact call succeeded - "+
			"if this never arrives, tools appended after the DAG five aren't guarded, or the corrected retry "+
			"couldn't proceed within the same turn", answer)
	}
}
