package tools

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

// planToolCtx supplies a nil ToolConfirmation (no pending confirm) - the
// functiontool runner consults it on every call, and StrictContextMock alone
// panics ("not implemented"). Mirrors hostpath_test.go's confirmlessCtx.
type planToolCtx struct{ *fakeCtx }

func (planToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// buildPlan drives create_plan then execute exactly as the model would -
// through Run, with JSON-shaped args - and returns the resulting cached
// plan. Shared by the #661 deterministic-setup tests below.
func buildPlan(t *testing.T, planner *dag.Planner, cache *PlanCache, githubSetup *dag.Setup, args map[string]any) dag.Plan {
	t.Helper()
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	createTl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt, ok := createTl.(runnableTool)
	if !ok {
		t.Fatalf("create_plan tool is not runnable")
	}
	cres, err := crt.Run(planToolCtx{newFakeCtx()}, args)
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	planID, _ := cres["plan_id"].(string)

	execTl, err := NewExecuteTool(planner, c, cache, nil, nil, nil, nil, "", nil, githubSetup, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	ert, ok := execTl.(runnableTool)
	if !ok {
		t.Fatalf("execute tool is not runnable")
	}
	if _, err := ert.Run(newExecToolCtx(), map[string]any{"plan_id": planID}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	p, ok := cache.Get(planID)
	if !ok {
		t.Fatalf("plan %q not found in cache", planID)
	}
	return p
}

// implementAssignment is a minimal, valid single-assignment create_plan
// payload for a code-implementer run - just enough to exercise Setup handling.
func implementAssignment() []map[string]any {
	return []map[string]any{{"agent": "code-implementer", "task": "implement the feature"}}
}

// TestExecuteToolStampsPlanOnly pins #739's plumbing half: execute stamps
// dag.Plan.PlanOnly from the harness-computed flag it's constructed with,
// never from anything the model submits (same as WorkerBackground/
// ContextItems) - carrying the quack:plan label's intent down to
// buildGateNodes, which enforces it.
func TestExecuteToolStampsPlanOnly(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	cache := NewPlanCache()
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")

	createTl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt := createTl.(runnableTool)
	cres, err := crt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": implementAssignment()})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	planID, _ := cres["plan_id"].(string)

	execTl, err := NewExecuteTool(planner, c, cache, nil, nil, nil, nil, "", nil, nil, nil, "", nil, true, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	ert := execTl.(runnableTool)
	if _, err := ert.Run(newExecToolCtx(), map[string]any{"plan_id": planID}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	p, ok := cache.Get(planID)
	if !ok {
		t.Fatalf("plan %q not found in cache", planID)
	}
	if !p.PlanOnly {
		t.Error("p.PlanOnly = false, want true - execute was constructed with planOnly=true")
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
	p := buildPlan(t, planner, NewPlanCache(), githubSetup, map[string]any{"assignments": implementAssignment()})
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

// TestGitHubSetupWorkBranchOverrideStillWins is issue #661's second test
// case, the PR-scoped half: a planner-supplied work_branch does not survive -
// the trigger's real PR head wins, with CheckoutExistingHead landing on top.
// repo/base_ref match the trigger here (a work_branch difference is
// legitimate - each dispatch can pick its own working branch); a
// repo/base_ref mismatch is a different, now-rejected case - see
// TestCreatePlanRejectsWholesaleMismatchedSetup.
func TestGitHubSetupWorkBranchOverrideStillWins(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	githubSetup := &dag.Setup{
		Repo:                 "https://github.com/fagerbergj/quack.git",
		BaseRef:              "main",
		WorkBranch:           "feat/real-pr-head", // the real PR head, as toDagSetup maps ExistingHeadRef
		CheckoutExistingHead: true,
	}
	args := map[string]any{
		"assignments": implementAssignment(),
		"setup": map[string]any{
			"repo": githubSetup.Repo, "base_ref": githubSetup.BaseRef,
			"work_branch": "planner-invented-branch",
		},
	}
	p := buildPlan(t, planner, NewPlanCache(), githubSetup, args)
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

// TestCreatePlanIgnoresWholesaleMismatchedSetup supersedes the QA rig's
// original owner rule (#slice3 review): a planner-invented setup.repo/
// base_ref that disagrees with the trigger's own used to be rejected
// outright, costing the model its whole (otherwise valid) plan over a field
// it can't actually change - the trigger's own setup always wins regardless
// (rec.Setup, TestGitHubSetupWorkBranchOverrideStillWins above). It's
// accepted and silently ignored now, noted in the summary instead.
func TestCreatePlanIgnoresWholesaleMismatchedSetup(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main", WorkBranch: "feat/real-pr-head"}
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	createTl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt := createTl.(runnableTool)
	args := map[string]any{
		"assignments": implementAssignment(),
		"setup": map[string]any{
			"repo": "https://example.com/planner-invented.git", "base_ref": "other",
			"work_branch": "planner-invented-branch",
		},
	}
	res, err := crt.Run(planToolCtx{newFakeCtx()}, args)
	if err != nil {
		t.Fatalf("create_plan Run: %v, want the mismatched setup ignored, not rejected", err)
	}
	summary, _ := res["summary"].(string)
	if !strings.Contains(summary, "setup ignored") {
		t.Errorf("summary = %q, want a setup-ignored note", summary)
	}
}

// TestNonGitHubRunKeepsPlannerSetup is issue #661's third test case: a plain
// (non-GitHub) run has no trigger Setup to draw from, so the planner's own
// declaration must pass through untouched.
func TestNonGitHubRunKeepsPlannerSetup(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	args := map[string]any{
		"assignments": implementAssignment(),
		"setup": map[string]any{
			"repo": "https://example.com/some-other-repo.git", "base_ref": "main",
			"work_branch": "feat/planner-chosen",
		},
	}
	p := buildPlan(t, planner, NewPlanCache(), nil, args)
	if p.Setup == nil {
		t.Fatal("plan.Setup is nil, want the planner's declared setup")
	}
	if p.Setup.WorkBranch != "feat/planner-chosen" || p.Setup.Repo != "https://example.com/some-other-repo.git" {
		t.Errorf("Setup = %+v, want the planner's own declared values", p.Setup)
	}
}

func TestNewCreatePlanToolMetadata(t *testing.T) {
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool error: %v", err)
	}
	if tl.Name() != "create_plan" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "create_plan")
	}
	// Every plan must declare setup + delivery, and the model must never run
	// git/push/PR itself - see github-delivery-architecture.
	for _, want := range []string{"setup", "delivery", "a node never pushes, opens a PR"} {
		if !strings.Contains(tl.Description(), want) {
			t.Errorf("Description() = %q, want it to mention %q", tl.Description(), want)
		}
	}
}

// summarizePlanRecord is the summary the model sees back after calling
// create_plan/edit_plan - it must surface the declared setup/delivery so the
// model can catch its own mistake before calling execute.
func TestSummarizePlanRecordIncludesSetupAndDelivery(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl", Task: "x"}},
		Setup:       &dag.Setup{BaseRef: "main", WorkBranch: "feat/widget"},
		Delivery:    &dag.Delivery{Kind: "pull_request"},
	}
	got := summarizePlanRecord(rec, map[string]string{"impl": "code-implementer"})
	for _, want := range []string{"feat/widget", "pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizePlanRecord = %q, want it to contain %q", got, want)
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
	emitPlanEvent(newFakeCtx(), plan, 1)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
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
	if got := attrs["quack.plan.step"].AsInt64(); got != 1 {
		t.Errorf("quack.plan.step = %d, want 1 - the dag_plan revision this step saved", got)
	}
}

// TestEmitPlanEvent_OmitsStepWhenNonPositive: a rejected step never reaches
// SaveStructured, so it has no revision to record - the ledger event must
// not claim step 0/negative as if it were a real one.
func TestEmitPlanEvent_OmitsStepAttributeWhenNonPositive(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	plan := &dag.Plan{ID: "plan-789", Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer"}}}
	emitPlanEvent(newFakeCtx(), plan, 0)

	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if _, ok := attrs["quack.plan.step"]; ok {
		t.Error("quack.plan.step present for step<=0, want it omitted")
	}
}

// TestEmitPlanEvent_RecordsInputMessages is issue #635: replaying a planning
// decision needs the ask alongside the plan - asserts the real Planner.Build
// fields (History, UserMessage) round-trip into gen_ai.input.messages, not a reconstruction from the plan's nodes; an inline-data attachment carries its mime type, never raw bytes (oversized gen_ai.input.messages risks OTel dropping the whole attribute).
func TestEmitPlanEvent_RecordsInputMessages(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	blobBytes := []byte("totally-secret-pixel-data")
	plan := &dag.Plan{
		ID:          "plan-456",
		Nodes:       []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		History:     []dag.HistoryTurn{{Role: "user", Text: "earlier turn"}},
		UserMessage: "fix the flaky test",
		Attachments: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: blobBytes}}},
	}
	emitPlanEvent(newFakeCtx(), plan, 1)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	var input string
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == "gen_ai.input.messages" {
			input = kv.Value.AsString()
		}
		return true
	})
	if input == "" {
		t.Fatal("gen_ai.input.messages missing")
	}
	for _, want := range []string{"earlier turn", "fix the flaky test", "image/png"} {
		if !strings.Contains(input, want) {
			t.Errorf("gen_ai.input.messages = %q, want it to contain %q (the planner's real ask)", input, want)
		}
	}
	if blob := base64.StdEncoding.EncodeToString(blobBytes); strings.Contains(input, blob) {
		t.Errorf("gen_ai.input.messages = %q, leaked the attachment's raw bytes", input)
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

// TestDagPlanEventCarriesContextWindow pins the context meter's static-limit
// path: DagPlanEvent must forward each node's ContextWindow (stamped by the
// planner from its agent's config) onto the wire DagNodeDef.
func TestDagPlanEventCarriesContextWindow(t *testing.T) {
	p := dag.Plan{ID: "p1", Nodes: []dag.Node{
		{ID: "a", AgentName: "web-researcher", ContextWindow: 131072},
		{ID: "b", AgentName: "synthesizer"}, // unconfigured limit -> 0
	}}
	ev := DagPlanEvent(context.Background(), p)
	data, ok := ev.Data.(stream.DagPlanData)
	if !ok {
		t.Fatalf("event data is %T, want stream.DagPlanData", ev.Data)
	}
	byID := map[string]stream.DagNodeDef{}
	for _, n := range data.Nodes {
		byID[n.ID] = n
	}
	if got := byID["a"].ContextWindow; got != 131072 {
		t.Errorf("node a ContextWindow = %d, want 131072", got)
	}
	if got := byID["b"].ContextWindow; got != 0 {
		t.Errorf("node b ContextWindow = %d, want 0", got)
	}
}

// TestDagPlanEventCarriesArtifact pins the #1178 wire path: DagPlanEvent must
// forward each node's declared output artifact kind (dag.Node.Artifact) onto the
// wire DagNodeDef so the frontend can pick the node's primary output exactly; a node declaring none carries an empty field.
func TestDagPlanEventCarriesArtifact(t *testing.T) {
	p := dag.Plan{ID: "p1", Nodes: []dag.Node{
		{ID: "a", AgentName: "code-explorer", Artifact: "text"},
		{ID: "b", AgentName: "synthesizer"}, // no declared output kind
	}}
	ev := DagPlanEvent(context.Background(), p)
	data, ok := ev.Data.(stream.DagPlanData)
	if !ok {
		t.Fatalf("event data is %T, want stream.DagPlanData", ev.Data)
	}
	byID := map[string]stream.DagNodeDef{}
	for _, n := range data.Nodes {
		byID[n.ID] = n
	}
	if got := byID["a"].Artifact; got != "text" {
		t.Errorf("node a Artifact = %q, want %q", got, "text")
	}
	if got := byID["b"].Artifact; got != "" {
		t.Errorf("node b Artifact = %q, want empty (undeclared)", got)
	}
}

// TestReviewDispatchSetupSatisfiesExistingHead pins the v0.29.0 cutover
// regression: a review-only dispatch declares its existing PR head via Setup
// (sdk ExistingHeadRef -> CheckoutExistingHead), so execute must take the
// head from the Setup, not reject every plan with "needs the PR's real head branch".
func TestReviewDispatchSetupSatisfiesExistingHead(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	githubSetup := &dag.Setup{
		Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main",
		WorkBranch: "quack/issue-836", CheckoutExistingHead: true,
	}
	reviewAssignment := []map[string]any{{"agent": "code-reviewer", "task": "review the PR"}}
	p := buildPlan(t, planner, NewPlanCache(), githubSetup, map[string]any{"assignments": reviewAssignment})
	if p.Setup == nil || p.Setup.WorkBranch != "quack/issue-836" || !p.Setup.CheckoutExistingHead {
		t.Errorf("Setup = %+v, want the dispatch-declared existing head checkout", p.Setup)
	}
}

// TestPlanTool_OriginFallbackSetupSatisfiesExistingHead is #1180's second
// defect: a nudge/retry dispatch carries no Run.Setup of its own, so serve's
// mergeExtOrigin resolves the chat's stored origin into a dag.Setup via WithGitHubSetup - indistinguishable at execute; pins that the fallback path is accepted as TestReviewDispatchSetupSatisfiesExistingHead pins the fresh path.
func TestPlanTool_OriginFallbackSetupSatisfiesExistingHead(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	// Stands in for what mergeExtOrigin(storedOriginJSON, nil, nil) returns
	// when a prior turn on this chat stored this Setup.
	fallbackSetup := &dag.Setup{
		Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main",
		WorkBranch: "quack/pr-1170", CheckoutExistingHead: true,
	}
	reviewAssignment := []map[string]any{{"agent": "code-reviewer", "task": "review the PR"}}
	p := buildPlan(t, planner, NewPlanCache(), fallbackSetup, map[string]any{"assignments": reviewAssignment})
	if p.Setup == nil || p.Setup.WorkBranch != "quack/pr-1170" || !p.Setup.CheckoutExistingHead {
		t.Errorf("Setup = %+v, want the origin-fallback PR head checked out as-is", p.Setup)
	}
}

// TestReviewWithoutExistingHeadStillRejected keeps #520's guard: a
// review-only plan whose dispatch Setup does NOT name an existing head must
// still be rejected by execute rather than reviewing a freshly-cut empty branch.
func TestReviewWithoutExistingHeadStillRejected(t *testing.T) {
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	githubSetup := &dag.Setup{
		Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main",
		WorkBranch: "quack/issue-836", // no CheckoutExistingHead
	}
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	createTl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt := createTl.(runnableTool)
	reviewAssignment := []map[string]any{{"agent": "code-reviewer", "task": "review the PR"}}
	cres, err := crt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": reviewAssignment})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	planID, _ := cres["plan_id"].(string)

	execTl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, nil, nil, nil, "", nil, githubSetup, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	ert := execTl.(runnableTool)
	if _, err := ert.Run(newExecToolCtx(), map[string]any{"plan_id": planID}); err == nil {
		t.Fatal("execute accepted, want the review-needs-real-head rejection")
	}
}
