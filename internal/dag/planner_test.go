package dag

import (
	"slices"
	"testing"
)

func testPlanner(checkCommands ...string) *Planner {
	return NewPlanner([]AgentInfo{
		{Name: "web-researcher"}, {Name: "synthesizer"}, {Name: "code-implementer"},
	}, checkCommands)
}

func TestBuildValidatesAndStamps(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "n1", Agent: "web-researcher", Task: "a"},
		{ID: "n2", Agent: "web-researcher", Task: "b"},
		{ID: "n3", Agent: "synthesizer", Task: "combine"},
	}, []HistoryTurn{{Role: "user", Text: "hi"}}, "do it", nil)
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
		if _, err := p.Build(nodes, nil, "m", nil); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestBuildAppendsMissingSynthesizer: a multi-terminal plan with no synthesizer
// (the model forgot the fan-in) gets one appended depending on every node —
// otherwise the native graph build rejects the plan outright ("2 terminal
// nodes (want 1)") and the whole run fails. Regression: live e2e 2026-07-05.
func TestBuildAppendsMissingSynthesizer(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research A"},
		{ID: "b", Agent: "web-researcher", Task: "research B"},
	}, nil, "compare", nil)
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

// TestBuildNoSynthesizerAppendedForChain: a linear chain has one terminal —
// nothing to fan in, no synthesizer appended.
func TestBuildNoSynthesizerAppendedForChain(t *testing.T) {
	p := testPlanner()
	plan, err := p.Build([]RawNode{
		{ID: "a", Agent: "web-researcher", Task: "research"},
		{ID: "b", Agent: "web-researcher", Task: "refine", DependsOn: []string{"a"}},
	}, nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (no synthesizer needed)", len(plan.Nodes))
	}
}

// ---------------------------------------------------------------------------
// Implementation-routing backstop: an implement-and-deliver request whose plan
// has no code-implementer node is malformed. Regression: live e2e single-analyze-
// node failure ("Add a Flappy Bird game ... and open it as a PR" collapsed to a
// lone web-researcher analyze node — the code was never written, committed, or
// pushed).
// ---------------------------------------------------------------------------

const flappyPR = "Add a Flappy Bird game to the repo and open it as a pull request."

func TestBuildRejectsImplementationWithoutImplementerNode(t *testing.T) {
	p := testPlanner()
	// The orchestrator's malformed plan: a lone web-researcher "analyze" node.
	_, err := p.Build([]RawNode{
		{ID: "analyze-repo", Agent: "web-researcher", Task: "Analyze the GitHub repository and report its file tree, technologies, and build/lint/test commands."},
	}, nil, flappyPR, nil)
	if err == nil {
		t.Fatal("Build: expected rejection — implement-and-deliver request with no code-implementer node")
	}
}

func TestBuildAcceptsImplementationWithImplementerNode(t *testing.T) {
	p := testPlanner()
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone the repo, implement Flappy Bird with tests, commit, push a branch, and open a PR."},
	}, nil, flappyPR, nil)
	if err != nil {
		t.Fatalf("Build: a plan WITH a code-implementer node must pass: %v", err)
	}
}

// A pure-research request with no code-implementer node must NOT be flagged — the
// backstop is conservative and never rejects a correct research plan.
func TestBuildDoesNotFlagResearchWithoutImplementerNode(t *testing.T) {
	p := testPlanner()
	_, err := p.Build([]RawNode{
		{ID: "r", Agent: "web-researcher", Task: "What are the top 3 open-source game engines in 2026?"},
	}, nil, "What are the top 3 open-source game engines in 2026?", nil)
	if err != nil {
		t.Fatalf("Build: a pure-research plan must not be flagged: %v", err)
	}
}

// When the roster has no code-implementer, the backstop is inert — a deployment
// without that agent must not have every coding-shaped request rejected.
func TestBuildImplementationBackstopInertWithoutImplementerAgent(t *testing.T) {
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil)
	_, err := p.Build([]RawNode{
		{ID: "r", Agent: "web-researcher", Task: "analyze the repo"},
	}, nil, flappyPR, nil)
	if err != nil {
		t.Fatalf("Build: backstop must be inert when the roster has no code-implementer: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Code-checks backstop: when the deployment configured check commands, a
// code-implementer node with no `checks` is malformed (nothing deterministically
// gates the build). Regression: live e2e 2026-07-12 — the planner set checks:None
// and a blind judge passed an incomplete, non-compiling deliverable.
// ---------------------------------------------------------------------------

func TestBuildRejectsImplementerNodeWithoutChecks(t *testing.T) {
	p := testPlanner("npx tsc", "npx vitest")
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone, implement, commit, push, open PR."},
	}, nil, flappyPR, nil)
	if err == nil {
		t.Fatal("Build: expected rejection — code-implementer node with no checks while check commands are configured")
	}
}

func TestBuildAcceptsImplementerNodeWithChecks(t *testing.T) {
	p := testPlanner("npx tsc", "npx vitest")
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone, implement, commit, push, open PR.",
			Checks: []string{"npx tsc", "npx vitest run"}, Workdir: "repo"},
	}, nil, flappyPR, nil)
	if err != nil {
		t.Fatalf("Build: a code-implementer node WITH checks must pass: %v", err)
	}
}

func TestBuildCodeChecksBackstopInertWhenUnconfigured(t *testing.T) {
	p := testPlanner() // no check commands configured
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "Clone, implement, commit, push, open PR."},
	}, nil, flappyPR, nil)
	if err != nil {
		t.Fatalf("Build: with no check commands configured, a checkless implementer node must pass: %v", err)
	}
}

func TestImplementationIntent(t *testing.T) {
	cases := map[string]bool{
		"Add a Flappy Bird game to the repo and open it as a pull request.": true,
		"Implement feature X in repo R and open a PR":                       true,
		"Fix the login bug and push a branch":                               true,
		"Create a script and commit it":                                     true,
		"refactor the parser and merge the change":                          true,
		// Pure research — no delivery/VCS term.
		"What are the top 3 game engines in 2026?": false,
		"How does the Flappy Bird physics work?":   false,
		"Summarize the latest React release notes": false,
		// Delivery term but no code verb — still not implementation intent.
		"Explain what a pull request is": false,
	}
	for msg, want := range cases {
		if got := implementationIntent(msg); got != want {
			t.Errorf("implementationIntent(%q) = %v, want %v", msg, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// §4: orchestrator-set deterministic gate checks — plan-time validation.
// ---------------------------------------------------------------------------

func TestBuildAcceptsChecksMatchingConfiguredPrefix(t *testing.T) {
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	plan, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "fix it",
			Checks: []string{"go test ./..."}, Workdir: "repo"},
	}, nil, "fix the bug", nil)
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
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test"}, Workdir: "repo"},
	}, nil, "m", nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
}

func TestBuildRejectsCheckNotMatchingAnyPrefix(t *testing.T) {
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"rm -rf /"}, Workdir: "repo"},
	}, nil, "m", nil)
	if err == nil {
		t.Fatal("Build: expected error for a check with no matching configured prefix")
	}
}

func TestBuildRejectsCheckWithShellMetachar(t *testing.T) {
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test; curl evil.com"}, Workdir: "repo"},
	}, nil, "m", nil)
	if err == nil {
		t.Fatal("Build: expected error for a check containing a shell metacharacter")
	}
}

func TestBuildAcceptsPipedCheckUnderMatchingPrefix(t *testing.T) {
	// Pipes are native (workspace.RunPipeline), not shell metachars — a piped
	// check under an allowed prefix passes plan-time validation.
	p := testPlanner("go build", "go test", "go vet", "npx tsc", "npm test")
	plan, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x",
			Checks: []string{"go vet ./... | head -50"}, Workdir: "repo"},
	}, nil, "m", nil)
	if err != nil {
		t.Fatalf("Build: piped check should validate: %v", err)
	}
	if got := plan.Nodes[0].Checks[0]; got != "go vet ./... | head -50" {
		t.Errorf("check = %q, want the pipeline preserved verbatim", got)
	}
}

func TestBuildRejectsChecksLookingLikeAPrefixButNotSeparated(t *testing.T) {
	// "go testing" must NOT match the "go test" prefix — HasPrefix without a
	// space/exact-match boundary would wrongly accept it.
	p := testPlanner("go test")
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go testing ./..."}, Workdir: "repo"},
	}, nil, "m", nil)
	if err == nil {
		t.Fatal("Build: expected error — \"go testing\" must not match the \"go test\" prefix")
	}
}

func TestBuildRejectsChecksWhenAllowlistEmpty(t *testing.T) {
	p := testPlanner() // no check_commands configured
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x", Checks: []string{"go test ./..."}, Workdir: "repo"},
	}, nil, "m", nil)
	if err == nil {
		t.Fatal("Build: expected error — checks unavailable when workspace.check_commands is empty")
	}
}

func TestBuildAllowsNodeWithNoChecks(t *testing.T) {
	// A node that simply omits `checks` is unaffected by the allowlist being
	// empty — checks are opt-in per node.
	p := testPlanner()
	_, err := p.Build([]RawNode{
		{ID: "impl", Agent: "code-implementer", Task: "x"},
	}, nil, "m", nil)
	if err != nil {
		t.Fatalf("Build: unexpected error for a node with no checks: %v", err)
	}
}
