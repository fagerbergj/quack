package dag

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
)

// captureLogExporter records every emitted log record.
type captureLogExporter struct{ records []sdklog.Record }

func (c *captureLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *captureLogExporter) Shutdown(context.Context) error   { return nil }
func (c *captureLogExporter) ForceFlush(context.Context) error { return nil }

func testPlanner(checkCommands ...string) *Planner {
	return NewPlanner([]AgentInfo{
		{Name: "web-researcher"}, {Name: "synthesizer"}, {Name: "code-implementer"},
	}, checkCommands, nil)
}

func TestBuildValidatesAndStamps(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a"},
		{ID: "n2", Agent: "web-researcher", Task: "b"},
		{ID: "n3", Agent: "synthesizer", Task: "combine"},
	}, nil, nil, []HistoryTurn{{Role: "user", Text: "hi"}}, "do it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.UserMessage != "do it" || len(plan.History) != 1 {
		t.Errorf("turn context not stamped: %+v", plan)
	}
	// Synthesizer hardening: n3 depends on all non-synth nodes even though we
	// supplied none.
	var synth Node
	for _, n := range plan.Nodes {
		if n.AgentName == "synthesizer" {
			synth = n
		}
	}
	if !slices.Equal(synth.DependsOn, []string{"n1", "n2"}) {
		t.Errorf("synthesizer depends_on = %v, want [n1 n2]", synth.DependsOn)
	}
}

func TestBuildStampsAgentContextWindow(t *testing.T) {
	p := NewPlanner([]AgentInfo{
		{Name: "web-researcher", ContextWindow: 131072},
		{Name: "synthesizer", ContextWindow: 65536},
	}, nil, nil)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a"},
		{ID: "s", Agent: "synthesizer", Task: "combine"},
	}, nil, nil, nil, "do it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, n := range plan.Nodes {
		byID[n.ID] = n
	}
	if got := byID["n1"].ContextWindow; got != 131072 {
		t.Errorf("n1.ContextWindow = %d, want 131072", got)
	}
	if got := byID["s"].ContextWindow; got != 65536 {
		t.Errorf("s.ContextWindow = %d, want 65536", got)
	}
}

func TestBuildRejectsBadPlans(t *testing.T) {
	p := testPlanner()
	cases := map[string][]RawNode{
		"empty":         {},
		"missing id":    {{Agent: "web-researcher", Task: "x"}},
		"unknown agent": {{ID: "n1", Agent: "nope", Task: "x"}},
		"duplicate id":  {{ID: "n1", Agent: "web-researcher"}, {ID: "n1", Agent: "web-researcher"}},
		"cycle":         {{ID: "n1", Agent: "web-researcher", DependsOn: []string{"n2"}}, {ID: "n2", Agent: "web-researcher", DependsOn: []string{"n1"}}},
	}
	for name, nodes := range cases {
		if _, err := p.Build(context.Background(), nodes, nil, nil, nil, "m", nil, nil); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// An unknown-agent rejection names every valid agent; retrying blind costs a planning round.
func TestBuildErrorsEnumerateValidOptions(t *testing.T) {
	p := testPlanner()
	cases := map[string]struct {
		nodes []RawNode
		want  []string
	}{
		"unknown agent": {
			nodes: []RawNode{{ID: "explore-core", Agent: "code-explementer", Task: "x"}},
			want:  []string{`"code-explementer"`, "code-implementer", "synthesizer", "web-researcher"},
		},
		"unknown depends_on": {
			nodes: []RawNode{{ID: "n1", Agent: "web-researcher", Task: "x", DependsOn: []string{"ghost"}}},
			want:  []string{`"ghost"`, "n1"},
		},
	}
	for name, tc := range cases {
		_, err := p.Build(context.Background(), tc.nodes, nil, nil, nil, "m", nil, nil)
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not contain %q", name, err.Error(), want)
			}
		}
	}
}

// A bound catalog entry renders into the exact Plan without any LLM call, plan judge included.
func TestBuildBoundProducesExactPlanWithoutJudge(t *testing.T) {
	judgeCalled := false
	judge := func(context.Context, string, string, string) (bool, string, error) {
		judgeCalled = true
		return true, "", nil
	}
	p := NewPlanner([]AgentInfo{{Name: "image-reader"}, {Name: "classifier"}}, nil, judge)
	nodes := []RawNode{
		{ID: "ocr", Agent: "image-reader", Task: "OCR scan-0042.pdf"},
		{ID: "classify", Agent: "classifier", Task: "Classify it.", DependsOn: []string{"ocr"}, Rubric: "names a folder"},
	}
	plan, err := p.BuildBound(context.Background(), nodes, nil, nil, "ingest scan-0042.pdf", nil, nil)
	if err != nil {
		t.Fatalf("BuildBound: %v", err)
	}
	if judgeCalled {
		t.Error("BuildBound invoked the plan judge; bound plans must skip it entirely")
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("plan.Nodes = %+v, want exactly the 2 bound nodes (no hardening/appending)", plan.Nodes)
	}
	if plan.Nodes[0].ID != "ocr" || plan.Nodes[0].AgentName != "image-reader" || plan.Nodes[0].Task != "OCR scan-0042.pdf" {
		t.Errorf("plan.Nodes[0] = %+v", plan.Nodes[0])
	}
	if plan.Nodes[1].ID != "classify" || plan.Nodes[1].Rubric != "names a folder" ||
		!slices.Equal(plan.Nodes[1].DependsOn, []string{"ocr"}) {
		t.Errorf("plan.Nodes[1] = %+v", plan.Nodes[1])
	}
	if plan.UserMessage != "ingest scan-0042.pdf" {
		t.Errorf("plan.UserMessage = %q", plan.UserMessage)
	}
}

// BuildBound reuses Build's structural checks; config-time validation is only a backstop.
func TestBuildBoundStillValidatesStructure(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "image-reader"}}, nil, nil)
	if _, err := p.BuildBound(context.Background(), []RawNode{
		{ID: "n1", Agent: "nope", Task: "x"},
	}, nil, nil, "m", nil, nil); err == nil {
		t.Error("expected an error for a bound node naming an unknown agent")
	}
}

// TestBuildKeepsIndependentSinks: "two researchers, no synthesizer" stays exactly that - every
// sink is delivered as its own section, so nothing is appended for the plan judge to reject.
func TestBuildKeepsIndependentSinks(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research A"},
		{ID: "b", Agent: "web-researcher", Task: "research B"},
	}, nil, nil, nil, "compare", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TerminalIDs(plan.Nodes); len(plan.Nodes) != 2 || !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("nodes = %d, sinks = %v, want the two researchers as sinks and nothing appended", len(plan.Nodes), got)
	}
}

// TestBuildAppendsSynthesizerForReviewFanout: 2+ reviewers and no synthesizer still get the fan-in,
// which stages the single overall verdict review automation waits on.
func TestBuildAppendsSynthesizerForReviewFanout(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-reviewer"}, {Name: "synthesizer", ContextWindow: 65536}}, nil, nil)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "r1", Agent: "code-reviewer", Task: "review A"}, {ID: "r2", Agent: "code-reviewer", Task: "review B"},
	}, nil, nil, nil, "review", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Nodes[len(plan.Nodes)-1]
	if len(plan.Nodes) != 3 || last.AgentName != "synthesizer" || !slices.Equal(last.DependsOn, []string{"r1", "r2"}) || last.ContextWindow != 65536 {
		t.Errorf("nodes = %+v, want a synthesizer fan-in over both reviewers", plan.Nodes)
	}
}

// TestBuildBoundRejectsMultipleSinks: a bound run has no plan record to fill a retried sink's siblings from.
func TestBuildBoundRejectsMultipleSinks(t *testing.T) {
	p := testPlanner()
	if _, err := p.BuildBound(context.Background(), []RawNode{
		{ID: "a", Agent: "web-researcher", Task: "A"}, {ID: "b", Agent: "web-researcher", Task: "B"},
	}, nil, nil, "m", nil, nil); err == nil || !strings.Contains(err.Error(), "2 terminal nodes") {
		t.Errorf("BuildBound err = %v, want the multi-sink refusal", err)
	}
}

// TestBuildNoSynthesizerAppendedForChain: a linear chain has one terminal -
// nothing to fan in, no synthesizer appended.
func TestBuildNoSynthesizerAppendedForChain(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research"},
		{ID: "b", Agent: "web-researcher", Task: "refine", DependsOn: []string{"a"}},
	}, nil, nil, nil, "x", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (no synthesizer needed)", len(plan.Nodes))
	}
}

// With no judge, a large PR reviewed by a lone code-reviewer is rejected and told to fan
// out; fanned-out plans, small PRs, and rosters without a code-explorer pass.
func TestReviewFanoutBackstop(t *testing.T) {
	roster := []AgentInfo{{Name: "code-explorer"}, {Name: "code-reviewer"}}
	p := NewPlanner(roster, nil, nil)
	largeMsg := "Review PR #3.\n\nChanged files (3):\n  a.ts (+600/-0)\n  b.ts (+400/-10)\n  c.ts (+50/-5)\n" // churn 1065 > 800
	smallMsg := "Review PR #7.\n\nChanged files (1):\n  a.ts (+100/-20)\n"                                    // churn 120

	if _, err := p.Build(context.Background(), []RawNode{{ID: "r", Agent: "code-reviewer", Task: "Review the PR and post."}}, nil, nil, nil, largeMsg, nil, nil); err == nil {
		t.Error("a lone code-reviewer for a large PR must be rejected (fan out expected)")
	}
	if _, err := p.Build(context.Background(), []RawNode{
		{ID: "e1", Agent: "code-explorer", Task: "Review a.ts, gather findings."},
		{ID: "e2", Agent: "code-explorer", Task: "Review b.ts and c.ts, gather findings."},
		{ID: "r", Agent: "code-reviewer", Task: "Validate the pooled findings and post.", DependsOn: []string{"e1", "e2"}},
	}, nil, nil, nil, largeMsg, nil, nil); err != nil {
		t.Errorf("a fanned-out large review (explorers → reviewer) must pass: %v", err)
	}
	if _, err := p.Build(context.Background(), []RawNode{{ID: "r", Agent: "code-reviewer", Task: "Review and post."}}, nil, nil, nil, smallMsg, nil, nil); err != nil {
		t.Errorf("a small PR as one reviewer node must pass: %v", err)
	}
	// No code-explorer in the roster ⇒ backstop inert (can't fan out).
	p2 := NewPlanner([]AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	if _, err := p2.Build(context.Background(), []RawNode{{ID: "r", Agent: "code-reviewer", Task: "Review and post."}}, nil, nil, nil, largeMsg, nil, nil); err != nil {
		t.Errorf("no code-explorer in the roster ⇒ backstop must be inert: %v", err)
	}
}

// With a judge wired, the churn backstop must not override a plan the judge accepted;
// it is only the fallback for judge-disabled deployments.
func TestReviewFanoutBackstopInertWhenJudgePresent(t *testing.T) {
	roster := []AgentInfo{{Name: "code-explorer"}, {Name: "code-reviewer"}}
	judge, _, _, _ := fakePlanJudge(true, "", nil)
	p := NewPlanner(roster, nil, judge)
	largeMsg := "Verify commit 8e50447 resolves the blocking finding; a scoped re-check of those three threads only.\n\n" +
		"Changed files (3):\n  a.ts (+600/-0)\n  b.ts (+400/-10)\n  c.ts (+50/-5)\n" // churn 1065 > 800

	if _, err := p.Build(context.Background(), []RawNode{
		{ID: "r", Agent: "code-reviewer", Task: "Verify commit 8e50447 resolves the blocking finding; re-check the three named threads only."},
	}, nil, nil, nil, largeMsg, nil, nil); err != nil {
		t.Errorf("a judge-accepted scoped single-reviewer plan on a large PR must pass: %v", err)
	}
}

// Checks are optional: the planner can only guess commands before seeing the repo, so the
// gate derives them from the clone (vetting.deriveChecks); a planner-set list still wins.

func TestBuildAcceptsImplementerNodeWithoutChecks(t *testing.T) {
	p := testPlanner("npx tsc", "npx vitest")
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone, implement, commit, push, open PR."},
	}, nil, nil, nil, "Add a Flappy Bird game to the repo and open it as a pull request.", nil, nil)
	if err != nil {
		t.Fatalf("Build: a code-implementer node with NO checks must be accepted (the gate derives them): %v", err)
	}
}

func TestBuildAcceptsImplementerNodeWithChecks(t *testing.T) {
	p := testPlanner("npx tsc", "npx vitest")
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone, implement, commit, push, open PR.",
			Checks: []string{"npx tsc", "npx vitest run"}, Workdir: "repo"},
	}, nil, nil, nil, "Add a Flappy Bird game to the repo and open it as a pull request.", nil, nil)
	if err != nil {
		t.Fatalf("Build: a code-implementer node WITH checks must pass: %v", err)
	}
}

// Plan-rubric judge: a fake vetting.PlanJudge proves the wiring (request/plan reach it,
// its verdict drives accept/reject, an error degrades gracefully).

// fakePlanJudge returns a vetting.PlanJudge that records the last request/plan
// summary it was called with and returns the canned verdict.
func fakePlanJudge(accept bool, reason string, callErr error) (judge vetting.PlanJudge, calls *int, lastRequest, lastSummary *string) {
	judge, calls, lastRequest, lastSummary, _ = fakePlanJudgeWithRepoKey(accept, reason, callErr)
	return judge, calls, lastRequest, lastSummary
}

// fakePlanJudgeWithRepoKey is fakePlanJudge plus the repoKey the judge was called with.
func fakePlanJudgeWithRepoKey(accept bool, reason string, callErr error) (judge vetting.PlanJudge, calls *int, lastRequest, lastSummary, lastRepoKey *string) {
	calls = new(int)
	lastRequest = new(string)
	lastSummary = new(string)
	lastRepoKey = new(string)
	judge = func(_ context.Context, request, planSummary, repoKey string) (bool, string, error) {
		*calls++
		*lastRequest = request
		*lastSummary = planSummary
		*lastRepoKey = repoKey
		return accept, reason, callErr
	}
	return judge, calls, lastRequest, lastSummary, lastRepoKey
}

// A plan that declares setup with a clone URL passes the judge the normalized repo key,
// not "" (which would force user-only recall).
func TestJudgeRoutingPassesRepoKeyFromDeclaredSetup(t *testing.T) {
	judge, _, _, _, lastRepoKey := fakePlanJudgeWithRepoKey(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: "code-reviewer"}}, nil, judge)
	setup := &Setup{Repo: "https://github.com/Acme/Widgets.git", BaseRef: "main", WorkBranch: "quack/issue-1"}
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: "code-reviewer", Task: "Review the PR."},
	}, setup, nil, nil, "review this PR", nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if *lastRepoKey != "github.com/acme/widgets" {
		t.Errorf("judge saw repoKey %q, want %q", *lastRepoKey, "github.com/acme/widgets")
	}
}

// A judge-accepted plan-only run (explore -> synthesize) passes even when its acceptance
// text mentions opening a PR later.
func TestJudgeRoutingAcceptsPlanOnlyPlan(t *testing.T) {
	judge, calls, lastRequest, lastSummary := fakePlanJudge(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}, {Name: "code-implementer"}}, nil, judge)
	msg := "Write a plan for adding a Flappy Bird game; the eventual implementation PR should follow repo conventions."
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Study the repo's conventions."},
		{ID: "write-plan", Agent: "synthesizer", Task: "Write the plan document.", DependsOn: []string{"explore"}},
	}, nil, nil, nil, msg, nil, nil)
	if err != nil {
		t.Fatalf("Build: a judge-accepted plan-only plan must pass: %v", err)
	}
	for _, n := range plan.Nodes {
		if n.AgentName == "code-implementer" {
			t.Errorf("plan-only plan must not carry a code-implementer node: %+v", n)
		}
	}
	if *calls != 1 {
		t.Fatalf("judge calls = %d, want 1", *calls)
	}
	if *lastRequest != msg {
		t.Errorf("judge saw request %q, want %q", *lastRequest, msg)
	}
	if *lastSummary == "" {
		t.Error("judge must receive a non-empty plan summary")
	}
}

// A judge rejection of an implement request with no implementer node surfaces the judge's
// reason so the re-plan loop can act on it.
func TestJudgeRoutingRejectsImplementWithoutImplementerNode(t *testing.T) {
	reason := "add a terminal code-implementer node"
	judge, calls, _, _ := fakePlanJudge(false, reason, nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, judge)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."},
	}, nil, nil, nil, "Add a Flappy Bird game to the repo and open it as a pull request.", nil, nil)
	if err == nil {
		t.Fatal("Build: expected rejection from the judge")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("Build error = %q, want it to carry the judge's reason %q", err, reason)
	}
	if *calls != 1 {
		t.Fatalf("judge calls = %d, want 1", *calls)
	}
}

// A review request (a plan with no code-reviewer node) rejected by the judge
// surfaces the same way.
func TestJudgeRoutingRejectsReviewWithoutReviewerNode(t *testing.T) {
	reason := "this is a review request; add a code-reviewer node"
	judge, _, _, _ := fakePlanJudge(false, reason, nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}, {Name: "code-reviewer"}}, nil, judge)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Summarize the PR diff."},
	}, nil, nil, nil, "Review PR #5 and post your findings as inline comments.", nil, nil)
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Build error = %v, want it to carry the judge's reason %q", err, reason)
	}
}

// A plan WITH the right reviewer node, accepted by the judge, passes.
func TestJudgeRoutingAcceptsReviewWithReviewerNode(t *testing.T) {
	judge, _, _, _ := fakePlanJudge(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: "code-reviewer"}}, nil, judge)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: "code-reviewer", Task: "Review PR #5 and post inline comments."},
	}, nil, nil, nil, "Review PR #5 and post your findings as inline comments.", nil, nil)
	if err != nil {
		t.Fatalf("Build: a judge-accepted review plan must pass: %v", err)
	}
}

// A nil judge (judge stage disabled) must never block plan validation - the
// dependency was never wired, so judgeRouting is a no-op.
func TestJudgeRoutingNoopWhenJudgeNil(t *testing.T) {
	p := testPlanner() // testPlanner wires judge=nil
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."},
	}, nil, nil, nil, "Add a Flappy Bird game to the repo and open it as a pull request.", nil, nil)
	if err != nil {
		t.Fatalf("Build: a nil judge must never block plan validation: %v", err)
	}
}

// A judge call error must degrade gracefully - allow the plan rather than
// wedge the run on the judge's own unavailability.
func TestJudgeRoutingDegradesGracefullyOnJudgeError(t *testing.T) {
	judge, calls, _, _ := fakePlanJudge(false, "", errors.New("judge model unreachable"))
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."},
	}, nil, nil, nil, "Add a Flappy Bird game to the repo and open it as a pull request.", nil, nil)
	if err != nil {
		t.Fatalf("Build: a judge error must degrade gracefully (allow), not block: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("judge calls = %d, want 1", *calls)
	}
}

// A rejection is a typed *PlanRejectedError, distinguishable without parsing error text.
func TestJudgeRoutingRejectionErrorTypeCarriesReason(t *testing.T) {
	reason := "add a terminal node that actually writes the plan"
	judge, _, _, _ := fakePlanJudge(false, reason, nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."},
	}, nil, nil, nil, "Write a plan.", nil, nil)
	var rejected *PlanRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Build error = %v (%T), want a *PlanRejectedError", err, err)
	}
	if rejected.Reason != reason {
		t.Errorf("PlanRejectedError.Reason = %q, want %q", rejected.Reason, reason)
	}
}

// TestJudgeRoutingWaived: a plan the user chose to run as is skips the judge that kept rejecting it.
func TestJudgeRoutingWaived(t *testing.T) {
	judge, calls, _, _ := fakePlanJudge(false, "no", nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	nodes := []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."}}
	if _, err := p.Build(WithPlanJudgeWaived(context.Background()), nodes, nil, nil, nil, "m", nil, nil); err != nil || *calls != 0 {
		t.Fatalf("waived Build err=%v judge calls=%d, want accepted without a judge call", err, *calls)
	}
	if _, err := p.Build(context.Background(), nodes, nil, nil, nil, "m", nil, nil); err == nil {
		t.Fatal("un-waived Build accepted a plan the judge rejects")
	}
}

// Every plan-judge rejection, not just the last, is recorded to the ledger with its reason.
func TestJudgeRoutingRejection_EmitsLedgerEventPerRejection(t *testing.T) {
	capExp := &captureLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	reasons := []string{"reason one: add a terminal node", "reason two: still no terminal node"}
	for _, reason := range reasons {
		judge, _, _, _ := fakePlanJudge(false, reason, nil)
		p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
		ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-693"})
		_, err := p.Build(ctx, []RawNode{
			{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."},
		}, nil, nil, nil, "Write a plan.", nil, nil)
		if err == nil {
			t.Fatalf("Build: expected rejection for reason %q", reason)
		}
	}

	var gotReasons []string
	for _, r := range capExp.records {
		var operation, explain string
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			switch string(kv.Key) {
			case otelobs.GenAIOperationName:
				operation = kv.Value.AsString()
			case otelobs.GenAIEvaluationExplain:
				explain = kv.Value.AsString()
			}
			return true
		})
		if operation == otelobs.GenAIOperationPlanRejected {
			gotReasons = append(gotReasons, explain)
		}
	}
	if len(gotReasons) != len(reasons) {
		t.Fatalf("ledger recorded %d plan_rejected events (%v), want %d (one per rejection)", len(gotReasons), gotReasons, len(reasons))
	}
	for i, reason := range reasons {
		if gotReasons[i] != reason {
			t.Errorf("ledger event %d reason = %q, want %q", i, gotReasons[i], reason)
		}
	}
}

// §4: orchestrator-set deterministic gate checks - plan-time validation.

func TestBuildAcceptsChecksMatchingConfiguredPrefix(t *testing.T) {
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "fix it",
			Checks: []string{"go test ./..."}, Workdir: "repo"},
	}, nil, nil, nil, "fix the bug", nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plan.Nodes[0].Checks; !slices.Equal(got, []string{"go test ./..."}) {
		t.Errorf("Checks = %v, want [go test ./...]", got)
	}
	if plan.Nodes[0].Workdir != "repo" {
		t.Errorf("Workdir = %q, want %q", plan.Nodes[0].Workdir, "repo")
	}
}

func TestBuildAcceptsCheckEqualToBarePrefix(t *testing.T) {
	p := testPlanner("go build", "go test")
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test"}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
}

func TestBuildRejectsCheckNotMatchingAnyPrefix(t *testing.T) {
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"rm -rf /"}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err == nil {
		t.Fatal("Build: expected error for a check with no matching configured prefix")
	}
}

func TestBuildAcceptsCheckWithQuotedMetachar(t *testing.T) {
	// Checks run shell-less, so metachars are literal argv; the prefix allowlist is the boundary.
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test -run 'Test(Foo)'"}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err != nil {
		t.Fatalf("Build: quoted-metachar check should validate: %v", err)
	}
	if got := plan.Nodes[0].Checks[0]; got != "go test -run 'Test(Foo)'" {
		t.Errorf("check = %q, want preserved verbatim", got)
	}
}

func TestBuildAcceptsPipedCheckUnderMatchingPrefix(t *testing.T) {
	// Pipes are native (workspace.RunPipeline), not shell metachars - a piped
	// check under an allowed prefix passes plan-time validation.
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x",
			Checks: []string{"go vet ./... | head -50"}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err != nil {
		t.Fatalf("Build: piped check should validate: %v", err)
	}
	if got := plan.Nodes[0].Checks[0]; got != "go vet ./... | head -50" {
		t.Errorf("check = %q, want the pipeline preserved verbatim", got)
	}
}

func TestBuildRejectsChecksLookingLikeAPrefixButNotSeparated(t *testing.T) {
	// "go testing" must NOT match the "go test" prefix - HasPrefix without a
	// space/exact-match boundary would wrongly accept it.
	p := testPlanner("go test")
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go testing ./..."}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err == nil {
		t.Fatal("Build: expected error - \"go testing\" must not match the \"go test\" prefix")
	}
}

func TestBuildRejectsChecksWhenAllowlistEmpty(t *testing.T) {
	p := testPlanner() // no check_commands configured
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test ./..."}, Workdir: "repo"},
	}, nil, nil, nil, "m", nil, nil)
	if err == nil {
		t.Fatal("Build: expected error - checks unavailable when workspace.check_commands is empty")
	}
}

// An unregistered artifact selector is rejected at build, naming the bad value, rather
// than failing at the gate with the output never saved.
func TestBuildRejectsUnregisteredArtifactKind(t *testing.T) {
	p := testPlanner()
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a", Artifact: "the one-word answer"},
	}, nil, nil, nil, "m", nil, nil)
	if err == nil {
		t.Fatal("Build: expected error for an unregistered artifact kind")
	}
	if !strings.Contains(err.Error(), `"the one-word answer"`) {
		t.Fatalf("Build error should name the bad value, got: %v", err)
	}
}

func TestBuildAcceptsRegisteredArtifactKind(t *testing.T) {
	p := testPlanner()
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a", Artifact: "document"},
	}, nil, nil, nil, "m", nil, nil)
	if err != nil {
		t.Fatalf("Build: unexpected error for a registered artifact kind: %v", err)
	}
}

func TestBuildAllowsNodeWithNoChecks(t *testing.T) {
	// A node that simply omits `checks` is unaffected by the allowlist being
	// empty - checks are opt-in per node.
	p := testPlanner()
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x"},
	}, nil, nil, nil, "m", nil, nil)
	if err != nil {
		t.Fatalf("Build: unexpected error for a node with no checks: %v", err)
	}
}

// Setup + Delivery - declared pre/post steps (github-delivery-architecture).

func TestBuildStampsSetupAndDeliveryOntoPlan(t *testing.T) {
	p := testPlanner()
	setup := &Setup{BaseRef: "main", WorkBranch: "feat/widget"}
	delivery := &Delivery{Kind: "pull_request"}
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "add the widget"},
	}, setup, delivery, nil, "add a widget and open a PR", nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Setup != setup {
		t.Errorf("Setup = %+v, want the passed-in setup", plan.Setup)
	}
	if plan.Delivery != delivery {
		t.Errorf("Delivery = %+v, want the passed-in delivery", plan.Delivery)
	}
}

func TestBuildAllowsNilSetupAndDelivery(t *testing.T) {
	// A plan with no GitHub repo involved (plain research) declares neither.
	p := testPlanner()
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "r", Agent: "web-researcher", Task: "research"},
	}, nil, nil, nil, "what's the weather", nil, nil)
	if err != nil {
		t.Fatalf("Build: nil setup/delivery must be accepted: %v", err)
	}
	if plan.Setup != nil || plan.Delivery != nil {
		t.Errorf("Setup/Delivery = %+v/%+v, want both nil", plan.Setup, plan.Delivery)
	}
}

func TestBuildRejectsUnknownDeliveryKind(t *testing.T) {
	p := testPlanner()
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x"},
	}, nil, &Delivery{Kind: "push_directly_to_main"}, nil, "m", nil, nil)
	if err == nil {
		t.Fatal("Build: expected error for a delivery.kind outside pull_request/review/comment")
	}
}

func TestBuildAcceptsEachValidDeliveryKind(t *testing.T) {
	p := testPlanner()
	for _, kind := range []string{"pull_request", "comment"} {
		if _, err := p.Build(context.Background(), []RawNode{
			{ID: "impl", Agent: "code-implementer", Task: "x"},
		}, nil, &Delivery{Kind: kind}, nil, "m", nil, nil); err != nil {
			t.Errorf("Build: delivery.kind %q must be accepted: %v", kind, err)
		}
	}
	// "review" additionally requires a reviewerAgent node (checkReviewDeliverable) -
	// covered on its own planner below, not the generic code-implementer fixture above.
	reviewPlanner := NewPlanner([]AgentInfo{{Name: reviewerAgent}}, nil, nil)
	if _, err := reviewPlanner.Build(context.Background(), []RawNode{
		{ID: "review", Agent: reviewerAgent, Task: "x"},
	}, nil, &Delivery{Kind: "review"}, nil, "m", nil, nil); err != nil {
		t.Errorf("Build: delivery.kind \"review\" with a %s node must be accepted: %v", reviewerAgent, err)
	}
}

// A partial step that merely allows review among its kinds has not committed to review
// delivery, so neither checkReviewDeliverable nor assemble's auto-fill may fire.
func TestBuildAllowsPartialReviewDispatchWhenDeliveryUndeclared(t *testing.T) {
	judge, calls, _, _ := fakePlanJudge(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: explorerAgent}, {Name: "synthesizer"}}, nil, judge)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: explorerAgent, Task: "Read the diff and form a verdict."},
	}, nil, nil, nil, "Review PR #888.", nil, []string{"review"})
	if err != nil {
		t.Fatalf("Build: a partial step with delivery undeclared must not be rejected for lacking a reviewer node yet: %v", err)
	}
	if *calls != 1 {
		t.Errorf("plan judge calls = %d, want 1 - the deterministic check must not short-circuit a legitimately partial plan", *calls)
	}
	if plan.Delivery != nil {
		t.Errorf("plan.Delivery = %+v, want nil - the judge must see the SAME undeclared delivery execute will", plan.Delivery)
	}
}

// The same dispatch, with a code-reviewer node added, passes.
func TestBuildAcceptsReviewDispatchWithReviewerNode(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: reviewerAgent}}, nil, nil)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: reviewerAgent, Task: "Review the diff and post inline comments."},
	}, nil, nil, nil, "Review PR #888.", nil, []string{"review", "comment"})
	if err != nil {
		t.Fatalf("Build: a review dispatch with a %s node must pass: %v", reviewerAgent, err)
	}
}

// A plan whose declared Delivery.Kind is "review" (rather than the dispatch
// grant) hits the same guard, independent of AllowedDeliveryKinds.
func TestBuildRejectsDeclaredReviewDeliveryWithoutReviewerNode(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: explorerAgent}}, nil, nil)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "explore", Agent: explorerAgent, Task: "Read the diff."},
	}, nil, &Delivery{Kind: "review"}, nil, "m", nil, nil)
	if err == nil || !strings.Contains(err.Error(), reviewerAgent) {
		t.Fatalf("Build error = %v, want rejection naming %q", err, reviewerAgent)
	}
}

// Non-review dispatches without a reviewer node keep passing.
func TestBuildAllowsNonReviewDispatchWithoutReviewerNode(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-implementer"}}, nil, nil)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Add the widget."},
	}, nil, &Delivery{Kind: "pull_request"}, nil, "Add a widget and open a PR.", nil, []string{"pull_request", "comment"})
	if err != nil {
		t.Fatalf("Build: a non-review dispatch must never require a %s node: %v", reviewerAgent, err)
	}
}

// Delivery is never inferred from omission: the judge and execute must agree on whether
// it was declared, even with a single allowed kind.
func TestBuildOmittedDeliveryStaysUndeclaredEvenWithSingleAllowedKind(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: reviewerAgent}}, nil, nil)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: reviewerAgent, Task: "Review the diff and post inline comments."},
	}, nil, nil, nil, "Review PR #888.", nil, []string{"review"})
	if err != nil {
		t.Fatalf("Build: a review-trigger plan with no declared delivery must validate: %v", err)
	}
	if plan.Delivery != nil {
		t.Fatalf("Delivery = %+v, want nil - omitting delivery must never be inferred, partial-plan support depends on it", plan.Delivery)
	}
}

// The explicit-but-kindless signal ({Kind: ""}) still resolves from a single-kind trigger.
func TestBuildResolvesKindlessDeliveryFromSingleAllowedKind(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: reviewerAgent}}, nil, nil)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: reviewerAgent, Task: "Review the diff and post inline comments."},
	}, nil, &Delivery{Kind: ""}, nil, "Review PR #888.", nil, []string{"review"})
	if err != nil {
		t.Fatalf("Build: a kindless delivery under a single-kind trigger must resolve: %v", err)
	}
	if plan.Delivery == nil || plan.Delivery.Kind != "review" {
		t.Fatalf("Delivery = %+v, want the trigger's single allowed kind resolved in", plan.Delivery)
	}
}

// A kindless delivery under a MULTI-kind trigger has nothing to infer from -
// this must be a clear rejection, not a silent "no delivery after all".
func TestBuildRejectsKindlessDeliveryUnderMultipleAllowedKinds(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: reviewerAgent}}, nil, nil)
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "review", Agent: reviewerAgent, Task: "Review the diff and post inline comments."},
	}, nil, &Delivery{Kind: ""}, nil, "Review PR #888.", nil, []string{"review", "comment"})
	if err == nil || !strings.Contains(err.Error(), "delivery.kind") {
		t.Fatalf("Build error = %v, want a delivery.kind rejection naming it can't be inferred", err)
	}
}

// A model-declared Delivery always wins over the trigger default.
func TestBuildModelDeliveryOverridesDefault(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-implementer"}}, nil, nil)
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x"},
	}, nil, &Delivery{Kind: "comment"}, nil, "m", nil, []string{"comment", "pull_request"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Delivery == nil || plan.Delivery.Kind != "comment" {
		t.Fatalf("Delivery = %+v, want the model's declared kind, not a default", plan.Delivery)
	}
}

func TestDefaultDeliveryFromAllowedKindsIgnoresMultipleOrUnknown(t *testing.T) {
	if d := DefaultDeliveryFromAllowedKinds(nil); d != nil {
		t.Errorf("no allowed kinds must not default: got %+v", d)
	}
	if d := DefaultDeliveryFromAllowedKinds([]string{"review", "comment"}); d != nil {
		t.Errorf("multiple allowed kinds must leave the choice to the model: got %+v", d)
	}
	if d := DefaultDeliveryFromAllowedKinds([]string{"push_directly_to_main"}); d != nil {
		t.Errorf("an unrecognized kind must not be defaulted: got %+v", d)
	}
}

// With a shared Setup clone, concurrent repo-touching nodes are rejected at build: they
// would corrupt the one working tree.
func TestBuildRejectsConcurrentRepoTouchingNodesWithSetup(t *testing.T) {
	p := testPlanner()
	setup := &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"}
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl1", Agent: "code-implementer", Task: "part one"},
		{ID: "impl2", Agent: "code-implementer", Task: "part two"},
	}, setup, &Delivery{Kind: "pull_request"}, nil, "do two independent things", nil, nil)
	if err == nil {
		t.Fatal("Build: expected an error - two repo-touching nodes with setup declared but no depends_on chain")
	}
	if !strings.Contains(err.Error(), "impl1") || !strings.Contains(err.Error(), "impl2") {
		t.Errorf("Build error = %q, want it to name both offending nodes", err)
	}
}

// A depends_on CHAIN of repo-touching nodes is exactly what setup's
// shared-branch design supports - Build must accept it.
func TestBuildAllowsChainedRepoTouchingNodesWithSetup(t *testing.T) {
	p := testPlanner()
	setup := &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"}
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "impl1", Agent: "code-implementer", Task: "part one"},
		{ID: "impl2", Agent: "code-implementer", Task: "part two", DependsOn: []string{"impl1"}},
	}, setup, &Delivery{Kind: "pull_request"}, nil, "do two sequential things", nil, nil)
	if err != nil {
		t.Fatalf("Build: a depends_on chain of repo-touching nodes must be accepted: %v", err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want exactly 2 (no synthesizer needed - one terminal already)", len(plan.Nodes))
	}
}

// Explorers are read-only, so parallel explorer nodes sharing a Setup clone are accepted.
func TestBuildAllowsConcurrentExplorerNodesWithSetup(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-explorer"}}, nil, nil)
	setup := &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"}
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "explore1", Agent: "code-explorer", Task: "survey the auth package"},
		{ID: "explore2", Agent: "code-explorer", Task: "survey the storage package"},
	}, setup, nil, nil, "explore two independent packages", nil, nil)
	if err != nil {
		t.Fatalf("Build: concurrent explorer nodes with setup must be accepted, not forced into a chain: %v", err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want exactly 2", len(plan.Nodes))
	}
}

// Reviewers get their own linked worktree, so parallel reviewers sharing a Setup clone are
// accepted; validateRepoChain orders only the implementer.
func TestBuildAllowsConcurrentReviewerNodesWithSetup(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	setup := &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"}
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "review1", Agent: "code-reviewer", Task: "review the auth changes"},
		{ID: "review2", Agent: "code-reviewer", Task: "review the storage changes"},
	}, setup, nil, nil, "review two independent changes", nil, nil)
	if err != nil {
		t.Fatalf("Build: concurrent reviewer nodes with setup must be accepted, not forced into a chain: %v", err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want exactly 2", len(plan.Nodes))
	}
}

// The implementer subset still needs its depends_on chain even with explorers alongside.
func TestBuildRejectsConcurrentImplementerNodesEvenWithExplorerPresent(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "code-implementer"}, {Name: "code-explorer"}}, nil, nil)
	setup := &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"}
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl1", Agent: "code-implementer", Task: "part one"},
		{ID: "impl2", Agent: "code-implementer", Task: "part two"},
		{ID: "explore", Agent: "code-explorer", Task: "survey the repo"},
	}, setup, &Delivery{Kind: "pull_request"}, nil, "do two independent things plus explore", nil, nil)
	if err == nil {
		t.Fatal("Build: expected an error - impl1/impl2 still need a depends_on chain, an unrelated explorer doesn't change that")
	}
}

// Without a declared Setup each repo-touching node gets its own clone, so no chain is needed.
func TestBuildAllowsConcurrentRepoTouchingNodesWithoutSetup(t *testing.T) {
	p := testPlanner()
	_, err := p.Build(context.Background(), []RawNode{
		{ID: "impl1", Agent: "code-implementer", Task: "part one"},
		{ID: "impl2", Agent: "code-implementer", Task: "part two"},
	}, nil, nil, nil, "do two independent things", nil, nil)
	if err != nil {
		t.Fatalf("Build: concurrent repo-touching nodes without setup must be accepted: %v", err)
	}
}

// planSummary is the judge's only view of the plan, so it must show setup/delivery or their absence.
func TestPlanSummaryIncludesSetupAndDelivery(t *testing.T) {
	plan := &Plan{
		Nodes:    []Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup:    &Setup{BaseRef: "main", WorkBranch: "feat/x"},
		Delivery: &Delivery{Kind: "pull_request"},
	}
	got := planSummary(plan)
	for _, want := range []string{"feat/x", "pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("planSummary = %q, want it to contain %q", got, want)
		}
	}
}

func TestPlanSummaryNotesAbsentSetupAndDelivery(t *testing.T) {
	plan := &Plan{Nodes: []Node{{ID: "r", AgentName: "web-researcher"}}}
	got := planSummary(plan)
	if !strings.Contains(got, "setup: (none declared)") || !strings.Contains(got, "delivery: (none declared") {
		t.Errorf("planSummary = %q, want it to note setup/delivery are absent", got)
	}
}

// The judge sees the whole growing plan each step, so an already-run node's result must
// appear in its summary.
func TestPlanSummaryShowsAlreadyRanResult(t *testing.T) {
	plan := &Plan{Nodes: []Node{
		{ID: "r", AgentName: "web-researcher", Task: "find the file", Result: "FOUND: internal/foo/bar.go defines it"},
		{ID: "impl", AgentName: "code-implementer", Task: "add the comment", DependsOn: []string{"r"}},
	}}
	got := planSummary(plan)
	if !strings.Contains(got, "ALREADY RAN") || !strings.Contains(got, "FOUND: internal/foo/bar.go defines it") {
		t.Errorf("planSummary = %q, want it to show r's already-ran result", got)
	}
	if strings.Contains(got, "impl") && strings.Contains(got[strings.Index(got, "- impl"):], "ALREADY RAN") {
		t.Errorf("planSummary = %q, want impl (never run) to carry no ALREADY RAN marker", got)
	}
}
