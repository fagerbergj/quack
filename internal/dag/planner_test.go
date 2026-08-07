package dag

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
)

// captureLogExporter records every emitted log record - a test-only sdklog.Exporter,
// mirrors internal/vetting's captureEvalExporter for the same reason (no shared
// export - each package's test double stays local to that package).
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

// TestBuildAppendsMissingSynthesizer: a multi-terminal plan with no synthesizer
// (the model forgot the fan-in) gets one appended depending on every node -
// otherwise the native graph build rejects the plan outright ("2 terminal
// nodes (want 1)") and the whole run fails. Regression: live e2e 2026-07-05.
func TestBuildAppendsMissingSynthesizer(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build(context.Background(), []RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research A"},
		{ID: "b", Agent: "web-researcher", Task: "research B"},
	}, nil, nil, nil, "compare", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (auto-appended synthesizer)", len(plan.Nodes))
	}
	synth := plan.Nodes[2]
	if synth.AgentName != "synthesizer" || !slices.Equal(synth.DependsOn, []string{"a", "b"}) {
		t.Errorf("appended node = %+v, want synthesizer depending on [a b]", synth)
	}
	if got := terminalIDs(plan.Nodes); len(got) != 1 {
		t.Errorf("terminals = %v, want exactly one", got)
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

// A large PR review planned as a LONE code-reviewer must be rejected and told to
// fan out (per-file-group explorers → one reviewer); a fanned-out plan, a small
// PR, and a roster without a code-explorer all pass. Judge is nil throughout -
// this backstop is the FALLBACK for a judge-disabled deployment (see
// TestReviewFanoutBackstopInertWhenJudgePresent for the judge-wired case).
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

// TestReviewFanoutBackstopInertWhenJudgePresent pins the PR-607 fix: with a
// judge wired, the mechanical churn count must NOT override a plan the judge
// already accepted - e.g. a lone code-reviewer node for a scoped re-check on a
// large PR. The judge (ask-fidelity aware, see plan_judge.go) is the
// authority on review sizing whenever one is available; the line-count
// backstop is only the fallback for a judge-disabled deployment.
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

// ---------------------------------------------------------------------------
// Checks are OPTIONAL. Regression (live e2e 2026-07-12): PR #180's checkCodeChecks
// backstop REJECTED any code-implementer node with empty `checks` whenever check
// commands were configured - but the planner authors the DAG before anything has
// looked at the repo, so it can only GUESS the commands (it guessed `go build` for
// a JavaScript repo). The run thrashed through 7 rejected plans and executed ZERO
// nodes. Checks are a property of the REPO: the trust gate now derives them from
// the cloned repo (vetting.deriveChecks); a planner-set list still wins.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Plan-rubric judge (judgeRouting) - replaces the old regex routing backstop.
// A fake vetting.PlanJudge stands in for the LLM so these tests don't need a
// live model; they prove the WIRING (request/plan reach the judge, its verdict
// drives accept/reject, and a judge error degrades gracefully) rather than any
// particular model's judgment.
// ---------------------------------------------------------------------------

// fakePlanJudge returns a vetting.PlanJudge that records the last request/plan
// summary it was called with and returns the canned verdict.
func fakePlanJudge(accept bool, reason string, callErr error) (judge vetting.PlanJudge, calls *int, lastRequest, lastSummary *string) {
	calls = new(int)
	lastRequest = new(string)
	lastSummary = new(string)
	judge = func(_ context.Context, request, planSummary string) (bool, string, error) {
		*calls++
		*lastRequest = request
		*lastSummary = planSummary
		return accept, reason, callErr
	}
	return judge, calls, lastRequest, lastSummary
}

// A plan-only run (explore → synthesize, no code-implementer) that the judge
// accepts must pass - the case the old regex heuristic mis-fired on (a
// plan-only issue whose acceptance text mentions "open a PR" for the EVENTUAL
// implementation, not this request).
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

// An implement-and-deliver request whose plan has only explorer nodes (no
// terminal code-implementer) must be rejected when the judge says so, with
// the judge's reason surfaced in the error so the orchestrator's re-plan loop
// can act on it.
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

// TestJudgeRoutingRejectionErrorTypeCarriesReason pins #693's plumbing: a
// rejection is a typed *PlanRejectedError (not a bare fmt.Errorf), so a caller
// can distinguish "the plan judge rejected this" from any other Build failure
// without parsing error text.
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

// TestJudgeRoutingRejection_EmitsLedgerEventPerRejection pins #693 test case 2:
// every plan-judge rejection - not just the last one - is recorded to the
// ledger with the judge's reason verbatim, independent of whatever the
// orchestrator eventually does with (or without) the plan.
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
		r.WalkAttributes(func(kv otellog.KeyValue) bool {
			switch kv.Key {
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

// ---------------------------------------------------------------------------
// §4: orchestrator-set deterministic gate checks - plan-time validation.
// ---------------------------------------------------------------------------

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
	// Metachars are no longer rejected (#277): checks run shell-less, so a quoted
	// regex with parens is literal argv under an allowed prefix. The prefix
	// allowlist - not a metachar scan - is the boundary.
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

// ---------------------------------------------------------------------------
// Setup + Delivery - declared pre/post steps (github-delivery-architecture).
// ---------------------------------------------------------------------------

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
	for _, kind := range []string{"pull_request", "review", "comment"} {
		if _, err := p.Build(context.Background(), []RawNode{
			{ID: "impl", Agent: "code-implementer", Task: "x"},
		}, nil, &Delivery{Kind: kind}, nil, "m", nil, nil); err != nil {
			t.Errorf("Build: delivery.kind %q must be accepted: %v", kind, err)
		}
	}
}

// #310: a plan that declares Setup (one shared clone+branch) but whose
// repo-touching nodes could run CONCURRENTLY (no depends_on between them)
// must be rejected at build - they'd corrupt the one shared working tree.
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

// #555: explorers are read-only, so PARALLEL explorer nodes sharing a
// plan.Setup clone must be accepted - unlike implementer/reviewer, they
// never mutate branch state, so there is nothing for concurrency to
// corrupt and no reason to force them into a chain.
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

// Worktree isolation: a reviewer no longer shares the shared clone
// directly - it gets its own linked git worktree - so PARALLEL reviewer nodes
// sharing a plan.Setup clone must be accepted exactly like parallel explorers
// above. Before worktree isolation this was rejected (reviewer was in the
// same repo-touching set as implementer); validateRepoChain now only orders
// the WRITER (implementer).
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

// Mixing a mutating repo-touching node with explorers: the implementer/
// reviewer subset still needs its depends_on chain even though the
// explorers running alongside them need none.
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

// Without a declared Setup, repo-touching nodes each get their OWN
// independent clone (unchanged pre-#310 behavior) - concurrent ones share
// nothing, so the chain requirement does not apply.
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

// The plan judge must see the declared setup/delivery (or their absence) so
// it can validate them against the request type - planSummary is the only
// channel it has into the plan.
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
	if !strings.Contains(got, "setup: (none declared)") || !strings.Contains(got, "delivery: (none declared)") {
		t.Errorf("planSummary = %q, want it to note setup/delivery are absent", got)
	}
}
