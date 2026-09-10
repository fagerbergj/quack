package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// countingRejectJudge rejects every plan with a distinct reason, numbered in call order.
func countingRejectJudge() vetting.PlanJudge {
	i := 0
	return func(context.Context, string, string, string) (bool, string, error) {
		i++
		return false, fmt.Sprintf("reason %d: add a terminal node", i), nil
	}
}

func planCalls(n int) []*model.LLMResponse {
	out := make([]*model.LLMResponse, n)
	for i := range out {
		out[i] = planCall()
	}
	return out
}

// TestOrchestrator_PlanJudgeCap_EndsTurnAtDefaultRoundCap: with no cap, a
// model that keeps calling plan after every rejection never stops on its
// own. The default round cap (5) must end the turn and deliver a failure
// carrying the rejection reasons. capAwareModel grants one real grace round
// after the cap trips (#760: a pivot must survive), so a model that keeps
// calling plan costs the round cap PLUS that one grace round in real
// invocations before the synthetic round ends the turn - 6 here, not 5.
func TestOrchestrator_PlanJudgeCap_EndsTurnAtDefaultRoundCap(t *testing.T) {
	stub := &orchStub{replies: planCalls(8)} // more than cap+grace - proves it actually stops there
	o := newTestOrchWithJudge(t, stub, countingRejectJudge())

	evs := runTurn(t, o, "review this PR")

	if got := stub.invocations(); got != 6 {
		t.Fatalf("model invocations = %d, want exactly 6 (the round cap of 5 plus one grace round)", got)
	}
	if !hasEvent(evs, stream.EventError) {
		t.Errorf("a capped run must surface an error event; events=%v", evs)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	const wantPrefix = "The planner could not produce an acceptable plan: "
	if !strings.HasPrefix(answer, wantPrefix) {
		t.Fatalf("answer = %q, want it to start with %q", answer, wantPrefix)
	}
	if answer == planExhaustedNotice {
		t.Fatal("a capped run must deliver the judge's reasons, not the generic reason-suppressing exhaustion notice")
	}
	for i := 1; i <= 5; i++ {
		want := fmt.Sprintf("reason %d:", i)
		if !strings.Contains(answer, want) {
			t.Errorf("answer missing round %d's reason: %q", i, answer)
		}
	}
}

// TestOrchestrator_PlanJudgeCap_Configurable proves SetPlanJudgeCap's round
// cap is honored, not just the built-in default of 5.
func TestOrchestrator_PlanJudgeCap_Configurable(t *testing.T) {
	stub := &orchStub{replies: planCalls(6)}
	o := newTestOrchWithJudge(t, stub, countingRejectJudge())
	o.SetPlanJudgeCap(2)

	runTurn(t, o, "review this PR")

	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want exactly the configured round cap (2) plus one grace round", got)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "reason 1:") || !strings.Contains(answer, "reason 2:") {
		t.Errorf("answer = %q, missing one of the two rounds' reasons", answer)
	}
}

// TestOrchestrator_PlanJudgeCap_GracePivotDeliveredVerbatim: once the round
// cap trips, the model gets exactly one more real round to react. A genuine
// pivot to a direct answer (#760) in that round must reach the user
// verbatim - it must NOT be replaced by the cap notice just because the
// round count is already high, and the round cap's own exhaustion check
// must not clobber it either.
func TestOrchestrator_PlanJudgeCap_GracePivotDeliveredVerbatim(t *testing.T) {
	const pivotAnswer = "This request doesn't need a plan - here's the direct answer."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), planCall(), // rounds 1-2: rejected, trips the round cap of 2
		stubText(pivotAnswer), // round 3 (grace): a genuine pivot, not another plan call
	}}
	o := newTestOrchWithJudge(t, stub, countingRejectJudge())
	o.SetPlanJudgeCap(2)

	evs := runTurn(t, o, "review this PR")

	if hasEvent(evs, stream.EventError) {
		t.Errorf("a grace-round pivot must not surface an error; events=%v", evs)
	}
	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want 3 (2 capped rounds + the one grace round)", got)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != pivotAnswer {
		t.Errorf("answer = %q, want the model's own grace-round pivot %q delivered verbatim", answer, pivotAnswer)
	}
}

// newTracedTestOrchWithJudge is newTracedTestOrch (ledger_coords_test.go)
// with a plan judge wired in - the orchestrator's own top-level model is
// traced like production's inference.NewModel/serve.go does, so a synthetic
// completion routed through the same wrapper (capAwareModel) is provably
// observable the same way a real one is.
func newTracedTestOrchWithJudge(t *testing.T, stub *orchStub, judge vetting.PlanJudge) *Orchestrator {
	t.Helper()
	tracedStub := inference.TracedModelForTesting(stub, "orch-test-model")
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
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, judge)
	return New(sessions, tracedStub, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestOrchestrator_PlanJudgeCap_SyntheticRoundRecordsLedgerEntry: the
// synthetic completion capAwareModel returns once the grace round is spent
// bypasses the real model - it must still leave a normal llm.call ledger
// trail (via inference.Traced), not vanish from observability just because
// no real backend was called.
func TestOrchestrator_PlanJudgeCap_SyntheticRoundRecordsLedgerEntry(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	stub := &orchStub{replies: planCalls(8)}
	o := newTracedTestOrchWithJudge(t, stub, countingRejectJudge())
	o.SetPlanJudgeCap(2)

	runTurn(t, o, "review this PR")

	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want 3 (2 capped rounds + one grace round)", got)
	}

	var synthetic map[string]string
	chatCalls := 0
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		if attrs["gen_ai.operation.name"] != "chat" {
			continue
		}
		chatCalls++
		if attrs["gen_ai.request.model"] == planJudgeCapModelName {
			synthetic = attrs
		}
	}
	if synthetic == nil {
		t.Fatal("no llm.call ledger event recorded for the synthetic round - it must be observable like a real call")
	}
	if got := synthetic["gen_ai.conversation.id"]; got != "chat" {
		t.Errorf("synthetic round's gen_ai.conversation.id = %q, want the chat id, same as a real call", got)
	}
	// 3 real rounds (2 capped + grace) plus the one synthetic completion.
	if chatCalls != 4 {
		t.Errorf("recorded %d chat ledger events, want 4 (3 real + 1 synthetic)", chatCalls)
	}
}
