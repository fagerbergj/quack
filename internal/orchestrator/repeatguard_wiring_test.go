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

// repeatLoopStub ignores every REFUSED error and re-issues the identical bad create_plan, so
// only the guard's hard-stop tier can bound it.
type repeatLoopStub struct{ calls int }

func (s *repeatLoopStub) Name() string { return "repeatLoopStub" }

func (s *repeatLoopStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.calls++
		yield(stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{"task": "x"}}}), nil)
	}
}

// The identical-call spam must end via the hard stop in exactly one invocation: a hard-stopped
// turn is never retried, though a soft refusal alone must not end the turn.
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
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)

	evs := runTurn(t, o, "do the flaky retry loop review")

	// tools.repeatThreshold+repeatHardStopAfter+1 (unexported): the hard stop fires on the call after
	// that sum.
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

// repeatWriteArtifactStub spams an appended tool (write_artifact), then corrects after the
// refusal, which must proceed within the same turn.
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

// The guard wraps the whole tool list, and a corrected call succeeds in the same turn: exactly 5
// stub calls (3 identical + 1 corrected + final text).
func TestOrchestratorRepeatGuardCoversAppendedTools(t *testing.T) {
	stub := &repeatWriteArtifactStub{}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, nil, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
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
