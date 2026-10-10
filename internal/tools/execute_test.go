package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// execToolCtx adds Actions(): execute's happy path sets SkipSummarization, which fakeCtx lacks.
type execToolCtx struct {
	planToolCtx
	actions session.EventActions
}

func newExecToolCtx() *execToolCtx { return &execToolCtx{planToolCtx: planToolCtx{newFakeCtx()}} }

// newFakeSetErrCtx: a ctx whose session State fails every Set.
func newFakeSetErrCtx() *fakeCtx {
	c := newFakeCtx()
	c.state.setErr = errors.New("state backend down")
	return c
}

func (c *execToolCtx) Actions() *session.EventActions { return &c.actions }

// capturePlan records the plan execute dispatches and starts every node, as a nil runStep would.
func capturePlan(got *dag.Plan) RunStepFunc {
	return func(_ context.Context, p dag.Plan, _ map[string]string, run map[string]bool) (map[string]string, map[string]bool, map[string]bool, error) {
		*got = p
		return nil, nil, run, nil
	}
}

// seedPlanRecord writes nodes then rec into a fresh store, as a prior create_plan/edit_plan would.
func seedPlanRecord(t *testing.T, rec dag.DagPlanRecord, nodes []dag.DagNodeRecord) *recordstore.Client {
	t.Helper()
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	for _, n := range nodes {
		if _, _, err := c.SaveStructured(context.Background(), "dag_node", n, n.NodeID, recordstore.Lineage{}); err != nil {
			t.Fatalf("seed dag_node %s: %v", n.NodeID, err)
		}
	}
	if _, _, err := c.SaveStructured(context.Background(), "dag_plan", rec, "", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_plan: %v", err)
	}
	return c
}

func TestNewExecuteToolMetadata(t *testing.T) {
	planner := dag.NewPlanner(nil, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, nil, nil, nil, "", nil, nil, nil, "", nil, false, "orchestrator", nil)
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

// An unreachable repo fails the execute tool call with an error the model can revise from, never a
// run-phase fatal.
func TestExecuteTool_UnreachableRepoReturnsHumanErrorNotFatal(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl", Task: "implement it"}},
		Setup:       &dag.Setup{Repo: "https://github.com/chrishay-quack/quack.git", BaseRef: "main", WorkBranch: "quack/work"},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "impl", Agent: "code-implementer"}})
	cache := NewPlanCache()

	// Mirrors the error shape SetupClone returns from runGit: "git <argv...>: <stderr>".
	gitFatal := errors.New("git clone --quiet --branch main --single-branch https://github.com/chrishay-quack/quack.git repo: " +
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	ex := dag.NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetSetup(func(context.Context, string, string, string, dag.Setup) error { return gitFatal })

	tl, err := NewExecuteTool(planner, c, cache, ex.Provision, nil, nil, nil, "implement it", nil, nil, nil, "", nil, false, "orchestrator", nil)
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
	// Nothing was selected, so the model can edit_plan and retry execute.
	if _, selected := cache.Selected(); selected {
		t.Error("a failed provisioning must not select the plan - the model needs to be able to retry")
	}
}

// execute provisions plan.Setup before marking the plan selected, and the dispatched plan carries
// Setup.Provisioned so the run phase skips it.
func TestExecuteTool_ProvisionsSetupBeforeSelecting(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl", Task: "implement it"}},
		Setup:       &dag.Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "impl", Agent: "code-implementer"}})
	cache := NewPlanCache()

	var provisionCalls int
	ex := dag.NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetSetup(func(context.Context, string, string, string, dag.Setup) error {
		provisionCalls++
		return nil
	})

	var got dag.Plan
	tl, err := NewExecuteTool(planner, c, cache, ex.Provision, capturePlan(&got), nil, nil, "implement it", nil, nil, nil, "", nil, false, "orchestrator", nil)
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
	if got.Setup == nil || !got.Setup.Provisioned {
		t.Error("dispatched plan's Setup.Provisioned = false after a successful execute, want true")
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

func TestDeliveredAnswer(t *testing.T) {
	never := func(string) bool { return false }
	answer := func(plan dag.Plan, outputs map[string]string, stopped func(string) bool) string {
		a, _ := DeliveredAnswer(plan, outputs, stopped)
		return a
	}
	// One sink: its output verbatim, never sectioned.
	single := dag.Plan{Nodes: []dag.Node{{ID: "n1"}}}
	if got, sectioned := DeliveredAnswer(single, map[string]string{"n1": "answer"}, never); got != "answer" || sectioned {
		t.Errorf("single node: got %q sectioned=%v, want %q", got, sectioned, "answer")
	}
	seq := dag.Plan{Nodes: []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}}}
	if got := answer(seq, map[string]string{"n1": "intermediate", "n2": "final"}, never); got != "final" {
		t.Errorf("sequential: got %q, want %q", got, "final")
	}
	if got := answer(single, map[string]string{}, never); got != "" {
		t.Errorf("empty outputs: got %q, want empty", got)
	}
	if got := answer(single, map[string]string{"n1": "draft"}, func(string) bool { return true }); got != "" {
		t.Errorf("stopped sink: got %q, want no answer", got)
	}
	// Two sinks: each its own section, in plan order, not map order or the first sink alone.
	two := dag.Plan{Nodes: []dag.Node{{ID: "b"}, {ID: "a"}}}
	if got, sectioned := DeliveredAnswer(two, map[string]string{"a": "A", "b": "B"}, never); got != "## b\n\nB\n\n## a\n\nA" || !sectioned {
		t.Errorf("two sinks: got %q sectioned=%v", got, sectioned)
	}
	if got := answer(two, map[string]string{"a": "", "b": "B"}, never); got != "## b\n\nB\n\n## a\n\n_No output._" {
		t.Errorf("a sink with no output: got %q, want its section marked", got)
	}
	if got := answer(two, map[string]string{"a": "A", "b": "B"}, func(string) bool { return true }); got != "" {
		t.Errorf("every sink stopped: got %q, want no answer", got)
	}
}

// ExecPlanKey is the cross-restart resume record, so a failed persist must fail the call rather than run
// on without it.
func TestExecuteTool_PlanPersistFailureSurfaces(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "r-1", Task: "find the file"}},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "r-1", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"r-1": "FOUND: file.go"}}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "find the file", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := &execToolCtx{planToolCtx: planToolCtx{newFakeSetErrCtx()}}
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err == nil {
		t.Fatalf("execute Run = %v, want a persist error", out)
	}
	if !strings.Contains(err.Error(), "persist plan for resume") {
		t.Errorf("err = %q, want it to name the plan persist", err)
	}
	if step.calls != 0 {
		t.Errorf("runStep called %d times after a persist failure, want 0", step.calls)
	}
	if _, selected := cache.Selected(); selected {
		t.Error("plan selected after a persist failure, want nothing persisted for a resume")
	}
}
