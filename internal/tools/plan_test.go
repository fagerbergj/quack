package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// planToolCtx supplies a nil ToolConfirmation (no pending confirm) - the
// functiontool runner consults it on every call, and StrictContextMock alone
// panics ("not implemented"). Mirrors hostpath_test.go's confirmlessCtx.
type planToolCtx struct{ *fakeCtx }

func (planToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// buildPlan runs the plan tool exactly as the model would - through Run, with
// JSON-shaped args - and returns the resulting cached plan. Shared by the
// #661 deterministic-setup tests below.
func buildPlan(t *testing.T, planner *dag.Planner, cache *PlanCache, existingHeadRef string, githubSetup *dag.Setup, args map[string]any) dag.Plan {
	t.Helper()
	tl, err := NewPlanTool(planner, cache, nil, nil, "", existingHeadRef, githubSetup, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewPlanTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("plan tool is not runnable")
	}
	res, err := rt.Run(planToolCtx{newFakeCtx()}, args)
	if err != nil {
		t.Fatalf("plan tool Run: %v", err)
	}
	planID, _ := res["plan_id"].(string)
	p, ok := cache.Get(planID)
	if !ok {
		t.Fatalf("plan %q not found in cache", planID)
	}
	return p
}

// implementNode is a minimal, valid single-node plan args payload for a
// code-implementer run - just enough to exercise Setup handling.
func implementNode() []map[string]any {
	return []map[string]any{{"id": "impl", "agent": "code-implementer", "task": "implement the feature", "depends_on": []string{}}}
}

// TestPlanToolStampsPlanOnly pins #739's plumbing half: the plan tool stamps
// dag.Plan.PlanOnly from the harness-computed flag it's constructed with,
// never from anything the model submits - the same way it already stamps
// WorkerBackground/CIChecks. This is what carries the quack:plan label's
// intent down to buildGateNodes, which is what actually enforces it.
func TestPlanToolStampsPlanOnly(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	cache := NewPlanCache()
	tl, err := NewPlanTool(planner, cache, nil, nil, "", "", nil, nil, "", nil, true)
	if err != nil {
		t.Fatalf("NewPlanTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("plan tool is not runnable")
	}
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"nodes": implementNode()})
	if err != nil {
		t.Fatalf("plan tool Run: %v", err)
	}
	planID, _ := res["plan_id"].(string)
	p, ok := cache.Get(planID)
	if !ok {
		t.Fatalf("plan %q not found in cache", planID)
	}
	if !p.PlanOnly {
		t.Error("p.PlanOnly = false, want true - the plan tool was constructed with planOnly=true")
	}
}

// TestGitHubSetupOverridesPlannerSetupNoRoundTrip is issue #661's first test
// case: an issue-implement run gets a deterministic work_branch even when the
// model never declares `setup` at all (no planner round-trip).
func TestGitHubSetupOverridesPlannerSetupNoRoundTrip(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	githubSetup := &dag.Setup{
		Repo:       "https://github.com/fagerbergj/quack.git",
		BaseRef:    "main",
		WorkBranch: "quack/issue-65",
	}
	p := buildPlan(t, planner, NewPlanCache(), "", githubSetup, map[string]any{"nodes": implementNode()})
	if p.Setup == nil {
		t.Fatal("plan.Setup is nil, want it filled from the trigger")
	}
	if p.Setup.WorkBranch != "quack/issue-65" {
		t.Errorf("WorkBranch = %q, want %q", p.Setup.WorkBranch, "quack/issue-65")
	}
	if p.Setup.Repo != githubSetup.Repo || p.Setup.BaseRef != githubSetup.BaseRef {
		t.Errorf("Setup = %+v, want repo/base_ref from the trigger %+v", p.Setup, githubSetup)
	}
}

// TestGitHubSetupWholesaleReplacesPlannerSetup is issue #661's second test
// case, the PR-scoped half: a planner-supplied setup (repo/base_ref/branch
// all different from the trigger's) must not survive - the trigger's values
// win entirely, and the existing-PR-head override still lands on top of them.
func TestGitHubSetupWholesaleReplacesPlannerSetup(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	githubSetup := &dag.Setup{
		Repo:       "https://github.com/fagerbergj/quack.git",
		BaseRef:    "main",
		WorkBranch: "quack/issue-97", // default, overridden below by the real PR head
	}
	args := map[string]any{
		"nodes": implementNode(),
		"setup": map[string]any{
			"repo": "https://example.com/planner-invented.git", "base_ref": "other",
			"work_branch": "planner-invented-branch",
		},
	}
	p := buildPlan(t, planner, NewPlanCache(), "feat/real-pr-head", githubSetup, args)
	if p.Setup == nil {
		t.Fatal("plan.Setup is nil")
	}
	if p.Setup.WorkBranch != "feat/real-pr-head" {
		t.Errorf("WorkBranch = %q, want the real PR head %q, not the planner's invented branch",
			p.Setup.WorkBranch, "feat/real-pr-head")
	}
	if !p.Setup.CheckoutExistingHead {
		t.Error("CheckoutExistingHead = false, want true for an existing PR head")
	}
	if p.Setup.Repo != githubSetup.Repo || p.Setup.BaseRef != githubSetup.BaseRef {
		t.Errorf("Setup = %+v, want repo/base_ref from the trigger, not the planner's", p.Setup)
	}
}

// TestNonGitHubRunKeepsPlannerSetup is issue #661's third test case: a plain
// (non-GitHub) run has no trigger Setup to draw from, so the planner's own
// declaration must pass through untouched.
func TestNonGitHubRunKeepsPlannerSetup(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	args := map[string]any{
		"nodes": implementNode(),
		"setup": map[string]any{
			"repo": "https://example.com/some-other-repo.git", "base_ref": "main",
			"work_branch": "feat/planner-chosen",
		},
	}
	p := buildPlan(t, planner, NewPlanCache(), "", nil, args)
	if p.Setup == nil {
		t.Fatal("plan.Setup is nil, want the planner's declared setup")
	}
	if p.Setup.WorkBranch != "feat/planner-chosen" || p.Setup.Repo != "https://example.com/some-other-repo.git" {
		t.Errorf("Setup = %+v, want the planner's own declared values", p.Setup)
	}
}

func TestNewPlanToolMetadata(t *testing.T) {
	planner := dag.NewPlanner(nil, nil, nil)
	tl, err := NewPlanTool(planner, NewPlanCache(), nil, nil, "", "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewPlanTool error: %v", err)
	}
	if tl.Name() != "plan" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "plan")
	}
	if !strings.Contains(tl.Description(), "DAG") {
		t.Errorf("Description() = %q, want mention of DAG", tl.Description())
	}
	// Every plan must declare setup + delivery, and the model must never run
	// git/push/PR itself - see github-delivery-architecture.
	for _, want := range []string{"setup", "delivery", "you never run git, push, or open a PR yourself"} {
		if !strings.Contains(tl.Description(), want) {
			t.Errorf("Description() = %q, want it to mention %q", tl.Description(), want)
		}
	}
}

// summarizePlan is the summary the model sees back after calling plan - it
// must surface the declared setup/delivery so the model can catch its own
// mistake before calling execute.
func TestSummarizePlanIncludesSetupAndDelivery(t *testing.T) {
	plan := &dag.Plan{
		Nodes:    []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup:    &dag.Setup{BaseRef: "main", WorkBranch: "feat/widget"},
		Delivery: &dag.Delivery{Kind: "pull_request"},
	}
	got := summarizePlan(plan)
	for _, want := range []string{"feat/widget", "pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizePlan = %q, want it to contain %q", got, want)
		}
	}
}

// recordCapture is a minimal sdklog.Exporter for direct record inspection.
type recordCapture struct{ records []sdklog.Record }

func (c *recordCapture) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *recordCapture) Shutdown(context.Context) error   { return nil }
func (c *recordCapture) ForceFlush(context.Context) error { return nil }

func TestEmitPlanEvent_ProducesWellFormedEvent(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	plan := &dag.Plan{ID: "plan-123", Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer"}}}
	emitPlanEvent(newFakeCtx(), plan)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]otellog.Value{}
	capExp.records[0].WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs["gen_ai.operation.name"].AsString(); got != "plan" {
		t.Errorf("gen_ai.operation.name = %q, want plan", got)
	}
	if got := attrs["gen_ai.workflow.name"].AsString(); got != "plan-123" {
		t.Errorf("gen_ai.workflow.name = %q, want plan-123", got)
	}
	if got := attrs["gen_ai.conversation.id"].AsString(); got != "sess" {
		t.Errorf("gen_ai.conversation.id = %q, want sess (from tc.SessionID())", got)
	}
	if attrs["gen_ai.output.messages"].AsString() == "" {
		t.Error("gen_ai.output.messages missing the marshaled plan")
	}
}

// TestPlanEdges verifies the wire edge list is derived from each node's
// DependsOn (From=dep, To=node), and a no-dependency node contributes nothing.
func TestPlanEdges(t *testing.T) {
	nodes := []dag.Node{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a", "b"}},
	}
	got := planEdges(nodes)
	want := []stream.DagEdgeDef{
		{From: "a", To: "b"},
		{From: "a", To: "c"},
		{From: "b", To: "c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planEdges = %+v, want %+v", got, want)
	}
}
