package orchestrator

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// neverCalledModel fails the test the instant it's asked to generate - used
// where a setup failure must abort the run before any node's worker runs.
type neverCalledModel struct{ t *testing.T }

func (neverCalledModel) Name() string { return "never-called" }

func (m neverCalledModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	m.t.Fatal("worker model must never be called - setup should have aborted the run first")
	return func(func(*model.LLMResponse, error) bool) {}
}

// TestRunBoundPlan_UnreachableRepoAbortsWithHumanErrorBeforeAnyNodeRuns pins
// #848's "bound workflows too" edge: a dispatch-bound plan skips the
// orchestrator LLM turn entirely (BuildBoundPlan -> RunBoundPlan), so there is
// no execute tool call to fail into. RunBoundPlan must provision the Setup
// itself, up front, and turn a clone failure into a human stream error -
// never a raw git dump, and never by letting a node start against an
// unprovisioned clone.
func TestRunBoundPlan_UnreachableRepoAbortsWithHumanErrorBeforeAnyNodeRuns(t *testing.T) {
	stub := neverCalledModel{t: t}
	ag, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "impl", Instruction: "ROLE Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"code-implementer": ag},
		map[string]model.LLM{"code-implementer": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{} }, nil)
	// Mirrors runGit's real error shape (internal/tools/git.go).
	ex.SetSetup(func(context.Context, string, string, string, dag.Setup) error {
		return errors.New("git clone --quiet --branch main https://github.com/chrishay-quack/quack.git repo: " +
			"fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	})
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)

	plan := dag.Plan{
		ID: "p1", UserMessage: "go",
		Setup: &dag.Setup{Repo: "https://github.com/chrishay-quack/quack.git", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer", Task: "do it"}},
	}

	var evs []stream.SSEEvent
	for ev, err := range o.RunBoundPlan(context.Background(), "u", "chat", SourceApp, plan) {
		if err != nil {
			t.Fatalf("RunBoundPlan iterator error: %v", err)
		}
		evs = append(evs, ev)
	}

	if !hasEvent(evs, stream.EventError) {
		t.Fatalf("want an error event on the stream, got none; events=%v", evs)
	}
	var msg string
	for _, ev := range evs {
		if ev.Name != stream.EventError {
			continue
		}
		if d, ok := ev.Data.(stream.ErrorData); ok {
			msg = d.Error
		}
	}
	if !strings.Contains(msg, "repository") || !strings.Contains(msg, "unreachable") || !strings.Contains(msg, "revise the plan") {
		t.Errorf("error event = %q, want the human setup-failure message", msg)
	}
	if strings.Contains(msg, "git clone") {
		t.Errorf("error event = %q, want the git argv dump stripped, not surfaced verbatim", msg)
	}
	if hasEvent(evs, stream.EventNodeStart) {
		t.Error("a node_start event fired - the run must abort BEFORE any node starts, setup already failed")
	}
}
