package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
)

// execToolCtx adds Actions() on top of planToolCtx - the execute tool's
// happy path sets SkipSummarization, which StrictContextMock (and so
// planToolCtx/fakeCtx) doesn't implement.
type execToolCtx struct {
	planToolCtx
	actions session.EventActions
}

func newExecToolCtx() *execToolCtx { return &execToolCtx{planToolCtx: planToolCtx{newFakeCtx()}} }

func (c *execToolCtx) Actions() *session.EventActions { return &c.actions }

func TestNewExecuteToolMetadata(t *testing.T) {
	tl, err := NewExecuteTool(NewPlanCache(), nil)
	if err != nil {
		t.Fatalf("NewExecuteTool error: %v", err)
	}
	if tl.Name() != "execute" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "execute")
	}
	if !strings.Contains(tl.Description(), "plan") {
		t.Errorf("Description() = %q, want mention of plan", tl.Description())
	}
}

// TestExecuteTool_UnreachableRepoReturnsHumanErrorNotFatal pins #848: a
// model-authored Setup naming an unreachable repo used to fail deep inside
// the run phase, killing the whole turn with a raw git fatal. It must instead
// fail the execute TOOL CALL - a normal error result the model sees and can
// revise from (drop setup, or name a reachable repo), never a fatal that ends
// the turn. Exercises the real dag.Executor.Provision (not a hand-rolled
// error) through the tool's actual Run path, mirroring dag/setup_test.go's
// fake-setupFn pattern for the git failure itself.
func TestExecuteTool_UnreachableRepoReturnsHumanErrorNotFatal(t *testing.T) {
	cache := NewPlanCache()
	plan := dag.Plan{
		ID:    "p1",
		Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup: &dag.Setup{Repo: "https://github.com/chrishay-quack/quack.git", BaseRef: "main", WorkBranch: "quack/work"},
	}
	cache.Put(plan)

	// Mirrors runGit's real error shape (internal/tools/git.go): "git
	// <argv...>: <stderr>" - what SetupClone actually returns.
	gitFatal := errors.New("git clone --quiet --branch main --single-branch https://github.com/chrishay-quack/quack.git repo: " +
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	ex := dag.NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetSetup(func(context.Context, string, string, string, dag.Setup) error { return gitFatal })

	tl, err := NewExecuteTool(cache, ex.Provision)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("execute tool is not runnable")
	}

	_, runErr := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": "p1"})
	if runErr == nil {
		t.Fatal("want an error when Setup provisioning fails, got nil")
	}
	msg := runErr.Error()
	if !strings.Contains(msg, "repository") || !strings.Contains(msg, "unreachable") || !strings.Contains(msg, "revise the plan") {
		t.Errorf("execute error = %q, want the human setup-failure message", msg)
	}
	if !strings.Contains(msg, "fatal: could not read Username") {
		t.Errorf("execute error = %q, want it to still carry the underlying git STDERR reason", msg)
	}
	if strings.Contains(msg, "git clone") {
		t.Errorf("execute error = %q, want the git argv dump stripped, not surfaced verbatim", msg)
	}
	// The turn must not die: nothing got selected, so the model can call plan
	// again with a corrected Setup and retry execute - this is what "feeds
	// back like every other tool error" means in practice.
	if _, selected := cache.Selected(); selected {
		t.Error("a failed provisioning must not select the plan - the model needs to be able to retry")
	}
}

// TestExecuteTool_ProvisionsSetupBeforeSelecting pins the happy path: the
// execute tool provisions plan.Setup itself (not just the run phase) before
// marking the plan selected, and the plan the caller reads back out of the
// cache carries Setup.Provisioned - so the run phase (runPlanSetup) skips it.
func TestExecuteTool_ProvisionsSetupBeforeSelecting(t *testing.T) {
	cache := NewPlanCache()
	plan := dag.Plan{
		ID:    "p1",
		Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup: &dag.Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
	}
	cache.Put(plan)

	var provisionCalls int
	ex := dag.NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetSetup(func(context.Context, string, string, string, dag.Setup) error {
		provisionCalls++
		return nil
	})

	tl, err := NewExecuteTool(cache, ex.Provision)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if provisionCalls != 1 {
		t.Fatalf("setupFn called %d times, want exactly 1", provisionCalls)
	}
	got, ok := cache.Get("p1")
	if !ok {
		t.Fatal("plan not found in cache")
	}
	if !got.Setup.Provisioned {
		t.Error("cached plan's Setup.Provisioned = false after a successful execute, want true")
	}
	if id, selected := cache.Selected(); !selected || id != "p1" {
		t.Errorf("Selected() = (%q, %v), want (\"p1\", true)", id, selected)
	}
}

func TestPlanCacheDelivered(t *testing.T) {
	c := NewPlanCache()
	if got := c.Delivered(); got != "" {
		t.Errorf("fresh cache Delivered() = %q, want empty", got)
	}
	c.SetDelivered("the verbatim answer")
	if got := c.Delivered(); got != "the verbatim answer" {
		t.Errorf("Delivered() = %q, want %q", got, "the verbatim answer")
	}
}

func TestTerminalOutput(t *testing.T) {
	// Single node - returns its output.
	single := dag.Plan{
		Nodes: []dag.Node{{ID: "n1"}},
	}
	if got := TerminalOutput(single, map[string]string{"n1": "answer"}); got != "answer" {
		t.Errorf("single node: got %q, want %q", got, "answer")
	}

	// Two nodes in sequence: n2 depends on n1, so n1 has a successor and n2 is terminal.
	seq := dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1"},
			{ID: "n2", DependsOn: []string{"n1"}},
		},
	}
	if got := TerminalOutput(seq, map[string]string{"n1": "intermediate", "n2": "final"}); got != "final" {
		t.Errorf("sequential: got %q, want %q", got, "final")
	}

	// Empty outputs - returns empty string (callers check for this).
	if got := TerminalOutput(single, map[string]string{}); got != "" {
		t.Errorf("empty outputs: got %q, want empty", got)
	}
}
