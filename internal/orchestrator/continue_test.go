package orchestrator

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// The orchestrator's continuation contract (live symptom, 2026-07-13): the
// orchestrator loads its skills, spends the rest of its output budget on
// reasoning, and ends the invocation with EMPTY content — no plan call, no
// execute call, no text. ADK reports a clean finish, so the run just stops: no
// DAG, no answer, no log line, chat back to idle. Same root cause as the
// worker's empty draft (PR #186): "the model emitted text" is not a completion
// signal. So: continue the orchestrator (bounded), and if it still produces
// nothing, FAIL LOUDLY.

// orchStub is a model.LLM that plays both the orchestrator (its request carries
// the plan tool) and the plan's worker/judge. The orchestrator's scripted
// replies come from replies, one per invocation; requests are recorded so a test
// can assert what the model actually SAW.
type orchStub struct {
	mu       sync.Mutex
	replies  []*model.LLMResponse // orchestrator turns, in order; last one repeats
	orchSaw  []string             // the user-visible text of each orchestrator request
	orchRuns int
}

func (*orchStub) Name() string { return "orchStub" }

func (s *orchStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case stubHasTool(req, "submit_verdict"): // the trust gate's judge
			yield(stubCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		case !stubHasTool(req, "plan"): // no plan tool ⇒ a plan worker
			yield(stubText("RESEARCH-RESULT"), nil)
			return
		}
		s.mu.Lock()
		s.orchSaw = append(s.orchSaw, stubUserText(req))
		i := s.orchRuns
		s.orchRuns++
		if i >= len(s.replies) {
			i = len(s.replies) - 1 // the last scripted reply repeats
		}
		reply := s.replies[i]
		s.mu.Unlock()
		// The plan tool mints the plan ID, so "commit the plan" can't be scripted
		// ahead of time: once a plan response is in the request, execute it.
		if id, ok := planIDFromRequest(req); ok {
			yield(executeCall(id), nil)
			return
		}
		yield(reply, nil)
	}
}

// invocations counts the model calls made as the ORCHESTRATOR (one per
// invocation of its llmagent turn — a tool call and its follow-up are two).
func (s *orchStub) invocations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orchRuns
}

func (s *orchStub) sawContinuation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.orchSaw {
		if strings.Contains(t, continuationMarker) {
			n++
		}
	}
	return n
}

// --- stub plumbing ---

func stubHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func stubUserText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func stubText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func stubCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// planCall is the orchestrator authoring a one-node DAG, the way it does live.
func planCall() *model.LLMResponse {
	return stubCall("plan", map[string]any{"nodes": []any{map[string]any{
		"id": "n1", "agent": "web-researcher", "task": "research the thing", "depends_on": []any{},
	}}})
}

// executeCall commits the plan the plan tool cached.
func executeCall(planID string) *model.LLMResponse {
	return stubCall("execute", map[string]any{"plan_id": planID})
}

// planIDFromRequest finds the plan tool's response in the request — the plan_id
// the model must hand to execute.
func planIDFromRequest(req *model.LLMRequest) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "plan" {
				continue
			}
			if id, ok := p.FunctionResponse.Response["plan_id"].(string); ok && id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// newTestOrch builds an Orchestrator over an in-memory session service, a real
// planner + executor (one web-researcher agent backed by the same stub), and the
// stub as the orchestrator's model.
func newTestOrch(t *testing.T, stub *orchStub) *Orchestrator {
	t.Helper()
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
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil)
	return New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil)
}

// runTurn drains a Run stream, returning the events it yielded.
func runTurn(t *testing.T, o *Orchestrator, msg string) []stream.SSEEvent {
	t.Helper()
	var evs []stream.SSEEvent
	for ev, err := range o.Run(context.Background(), "u", "chat", msg, nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func hasEvent(evs []stream.SSEEvent, name string) bool {
	for _, ev := range evs {
		if ev.Name == name {
			return true
		}
	}
	return false
}

// sessionHasContinuation reports whether the continuation directive was
// delivered as a SESSION EVENT — the only delivery an llmagent actually reads
// (it rebuilds its request from Session().Events(); see [[adk-ignores-usercontent]]).
func sessionHasContinuation(t *testing.T, o *Orchestrator) bool {
	t.Helper()
	for _, ev := range o.PriorEvents(context.Background(), "u", "chat") {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && strings.Contains(p.Text, continuationMarker) {
				return true
			}
		}
	}
	return false
}

// TestOrchestrator_EmptyTurn_ContinuedAndRuns: the first invocation returns
// EMPTY (the live symptom) — the orchestrator is re-invoked with the
// continuation directive, and the plan it then authors runs to an answer.
func TestOrchestrator_EmptyTurn_ContinuedAndRuns(t *testing.T) {
	stub := &orchStub{replies: []*model.LLMResponse{
		stubText(""), // turn 1: EMPTY — no plan, no execute, no text (the live symptom)
		planCall(),   // turn 2, having seen the continuation: author the DAG
		// turn 3 (execute) is not scripted: the stub commits the plan as soon as
		// the plan tool's response — with its minted plan_id — is in the request.
	}}
	o := newTestOrch(t, stub)

	evs := runTurn(t, o, "research the thing")

	if got := stub.invocations(); got < 2 {
		t.Fatalf("orchestrator invoked %d times, want at least 2 (empty turn ⇒ continuation)", got)
	}
	if stub.sawContinuation() == 0 {
		t.Errorf("the continuation directive never reached the model; requests=%q", stub.orchSaw)
	}
	if !sessionHasContinuation(t, o) {
		t.Errorf("the continuation directive was not delivered as a session event")
	}
	if hasEvent(evs, stream.EventError) {
		t.Errorf("the run recovered — it must not surface an error; events=%v", evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); !strings.Contains(answer, "RESEARCH-RESULT") {
		t.Errorf("plan answer = %q, want the node's output — the run did not proceed", answer)
	}
}

// TestOrchestrator_AlwaysEmpty_FailsLoud: an orchestrator that produces nothing
// on EVERY attempt must not die silently — the retry budget is spent, an error
// reaches the user, and the failure is logged at ERROR.
func TestOrchestrator_AlwaysEmpty_FailsLoud(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	stub := &orchStub{replies: []*model.LLMResponse{stubText("")}} // empty, forever
	o := newTestOrch(t, stub)

	evs := runTurn(t, o, "research the thing")

	if got, want := stub.invocations(), 1+maxOrchestratorContinues; got != want {
		t.Errorf("orchestrator invoked %d times, want %d (the first turn + the bounded retry budget)", got, want)
	}
	if !hasEvent(evs, stream.EventError) {
		t.Errorf("a run that produced NOTHING must surface an error, not end silently; events=%v", evs)
	}
	if !strings.Contains(logs.String(), "orchestrator") {
		t.Errorf("the silent death must be logged at ERROR; logs=%q", logs.String())
	}
}

// TestOrchestrator_NormalTurn_Untouched: an orchestrator that answers on the
// first try is invoked exactly once and never sees a continuation.
func TestOrchestrator_NormalTurn_Untouched(t *testing.T) {
	stub := &orchStub{replies: []*model.LLMResponse{stubText("Ducks are birds.")}}
	o := newTestOrch(t, stub)

	evs := runTurn(t, o, "are ducks birds?")

	if got := stub.invocations(); got != 1 {
		t.Errorf("orchestrator invoked %d times, want exactly 1 (a normal turn must not be continued)", got)
	}
	if stub.sawContinuation() != 0 {
		t.Errorf("a normal turn must never see a continuation directive")
	}
	if sessionHasContinuation(t, o) {
		t.Errorf("a normal turn must not write a continuation event into the session")
	}
	if hasEvent(evs, stream.EventError) {
		t.Errorf("a normal turn must not surface an error; events=%v", evs)
	}
}
