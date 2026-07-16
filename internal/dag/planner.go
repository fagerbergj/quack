package dag

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// implementerAgent is the well-known bundle name (config key agents/code-implementer)
// of the only specialist that can change, commit, and push code. Referenced by name
// the same way assemble() hardcodes "synthesizer" — the roster is name-keyed and both
// roles carry a fixed contract the plan validation depends on.
const implementerAgent = "code-implementer"
const reviewerAgent = "code-reviewer"
const explorerAgent = "code-explorer"

// reviewChurnThreshold is the changed-line count above which a single
// code-reviewer node reliably chokes on the whole diff (compaction churn +
// slow re-diffing — a live +1271-line PR stalled for 30+ min). Above it, the
// review must fan out into per-file-group explorers feeding one reviewer.
const reviewChurnThreshold = 800

// changedChurnRe matches the "(+add/-del)" churn markers the webhook's
// changed-files summary renders per file, so the backstop can size a PR from the
// run message without threading the file list through the planner.
var changedChurnRe = regexp.MustCompile(`\(\+(\d+)/-(\d+)\)`)

// totalChurn sums the added+deleted lines named in the run message's
// changed-files summary; 0 when the message carries no such summary.
func totalChurn(message string) int {
	sum := 0
	for _, m := range changedChurnRe.FindAllStringSubmatch(message, -1) {
		a, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		sum += a + d
	}
	return sum
}

// AgentInfo describes one available agent (name + description) — the roster the
// orchestrator authors a DAG from.
type AgentInfo struct {
	Name        string
	Description string
}

// Planner validates an orchestrator-authored DAG and stamps the turn's context
// (verbatim message, history, attachments) onto it for the executor. There is no
// LLM here: the orchestrator authors the DAG itself, guided by the plan-work
// skill. This checks it — known agents, unique ids, acyclic — and hardens the
// synthesizer's dependencies.
type Planner struct {
	agents []AgentInfo
	// checkCommands are the allowed check-command PREFIXES (workspace.
	// check_commands) a node's `checks` may complete into — see §4 of
	// .quack/plan-pr5-tool-schemas.md. Empty (default) means checks are
	// unavailable: any node that sets `checks` is rejected at plan time.
	checkCommands []string
	// judge scores a proposed plan against the plan-quality rubric
	// (vetting.PlanJudge) — replaces the old regex routing backstop. nil when
	// the judge stage is disabled (config.Gates.JudgeEnabled() == false):
	// judgeRouting then no-ops rather than blocking plan validation on a
	// dependency that was never wired.
	judge vetting.PlanJudge
}

// NewPlanner returns a Planner over the available agent roster, the
// configured check-command prefixes (workspace.check_commands; may be empty),
// and the plan judge (may be nil — see Planner.judge).
func NewPlanner(agents []AgentInfo, checkCommands []string, judge vetting.PlanJudge) *Planner {
	return &Planner{agents: agents, checkCommands: checkCommands, judge: judge}
}

// CheckCommands returns the configured check-command prefixes (may be empty).
// The plan tool's description (internal/tools/plan.go) reads this to tell the
// orchestrator model what's available, so a plan node's `checks` are filled
// in against operator-approved prefixes rather than invented.
func (p *Planner) CheckCommands() []string { return p.checkCommands }

// RawNode is one DAG node the orchestrator submits to the plan tool.
type RawNode struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	Rubric    string   `json:"rubric,omitempty"`
	DependsOn []string `json:"depends_on"`
	// Checks are orchestrator-set deterministic gate commands (§4): each MUST
	// prefix-match a configured workspace.check_commands entry and contain no
	// shell metacharacters — validated at plan time (assemble/validateChecks),
	// never at run time. Typically set only on code-implementer nodes.
	Checks []string `json:"checks,omitempty"`
	// Workdir is the workspace-relative directory Checks run in (the node's
	// repo). Ignored when Checks is empty.
	Workdir string `json:"workdir,omitempty"`
}

// Build validates the submitted nodes into a Plan and stamps the turn context.
// message is the verbatim user request, history the prior turns, attachments the
// current media — all threaded to every node by the executor. ctx bounds the
// (optional) plan-judge call. Returns an error (no silent fallback) so the
// orchestrator can fix and re-submit.
func (p *Planner) Build(ctx context.Context, nodes []RawNode, history []HistoryTurn, message string, attachments []*genai.Part) (*Plan, error) {
	plan, err := assemble(nodes, p.agents, p.checkCommands)
	if err != nil {
		return nil, err
	}
	if err := p.judgeRouting(ctx, plan, message); err != nil {
		return nil, err
	}
	if err := p.checkReviewFanout(plan, message); err != nil {
		return nil, err
	}
	plan.History = history
	plan.UserMessage = message
	plan.Attachments = attachments
	return plan, nil
}

// judgeRouting replaces the old regex routing backstop (checkImplementationRouting)
// with a small rubric judged by the existing judge (vetting.PlanJudge): the
// judge reads the proposed plan against the user's actual request and scores
// its SHAPE — right terminal deliverable, addresses the ask, grounded, minimal
// decomposition, verifiable — so intent comes from context rather than
// verb/delivery-term string matching, which mis-fired on a plan-only run whose
// injected acceptance text ("open a PR") described the EVENTUAL implementation,
// not this request.
//
// Graceful degradation: judge==nil (judge stage disabled) or a judge call error
// both ALLOW the plan rather than blocking it — a routing check must never wedge
// a run on its own failure. Only an explicit reject from a judge that actually
// ran turns into an error, so the orchestrator's existing re-plan loop is the
// retry budget (same contract checkImplementationRouting had).
func (p *Planner) judgeRouting(ctx context.Context, plan *Plan, message string) error {
	if p.judge == nil {
		return nil
	}
	accept, reason, err := p.judge(ctx, message, planSummary(plan))
	if err != nil {
		slog.Warn("plan judge unavailable, allowing plan", "component", "planner", "error", err)
		return nil
	}
	if accept {
		return nil
	}
	slog.Warn("plan rejected by plan judge", "component", "planner", "reason", reason, "message", message)
	return fmt.Errorf("this plan was rejected: %s\nFix the nodes and call plan again.", reason)
}

// planSummary renders the plan for the plan judge: each node's id, agent,
// dependencies, and full task text — enough for the judge to assess the
// decomposition and terminal deliverable without re-running anything.
func planSummary(p *Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d node(s):", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(n.Task))
	}
	return sb.String()
}

// checkReviewFanout is the deterministic backstop for a large PR review planned as
// a SINGLE code-reviewer node: the whole diff lands in one agent's context, which
// churns compaction and re-diffs slowly (a +1271-line PR stalled 30+ min). When
// the run message reports churn above reviewChurnThreshold and the plan has a
// reviewer but NO explorer to spread the reading across, reject with a targeted
// fix — slice the changed files into per-group explorers feeding the one reviewer.
// Inert when the roster has no code-explorer, or when the plan already fans out.
func (p *Planner) checkReviewFanout(plan *Plan, message string) error {
	hasExplorer := false
	for _, a := range p.agents {
		if a.Name == explorerAgent {
			hasExplorer = true
			break
		}
	}
	if !hasExplorer || totalChurn(message) < reviewChurnThreshold {
		return nil
	}
	var reviewers, explorers int
	for _, n := range plan.Nodes {
		switch n.AgentName {
		case reviewerAgent:
			reviewers++
		case explorerAgent:
			explorers++
		}
	}
	if reviewers == 0 || explorers > 0 {
		return nil // not a review plan, or already fanned out
	}
	slog.Warn("plan rejected: large PR review not fanned out",
		"component", "planner", "churn", totalChurn(message), "threshold", reviewChurnThreshold)
	return fmt.Errorf("this PR is large (%d changed lines, over the %d-line threshold) and a single %s node will "+
		"choke on the whole diff. Split the review: read the changed-files list in the request, group the files into "+
		"slices of roughly 300 changed lines each, and add ONE %s node per slice (its task: review ONLY the files in "+
		"its slice, gather findings, do NOT post). Keep ONE %s node that depends on all the explorers, validates their "+
		"pooled findings against the diff, and posts. Do NOT keep a lone %s node.",
		totalChurn(message), reviewChurnThreshold, reviewerAgent, explorerAgent, reviewerAgent, reviewerAgent)
}

// AttachmentDesc returns a human-readable description of the attachment list
// (e.g. "[User attached: 2 file(s): image/jpeg, audio/mp3]") so the text-only
// orchestrator knows media is present and routes to a media-capable agent.
func AttachmentDesc(parts []*genai.Part) string {
	if len(parts) == 0 {
		return ""
	}
	var mimes []string
	for _, p := range parts {
		if p.InlineData != nil && p.InlineData.MIMEType != "" {
			mimes = append(mimes, p.InlineData.MIMEType)
		}
	}
	if len(mimes) == 0 {
		return ""
	}
	return fmt.Sprintf("[User attached: %d file(s): %s]", len(mimes), strings.Join(mimes, ", "))
}

// assemble validates nodes against the agent roster, hardens the synthesizer's
// dependencies, and checks acyclicity.
func assemble(nodes []RawNode, agents []AgentInfo, checkCommands []string) (*Plan, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("plan has no nodes")
	}
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a.Name] = true
	}
	ids := make(map[string]bool, len(nodes))
	plan := &Plan{ID: uuid.NewString()}
	for _, n := range nodes {
		if n.ID == "" {
			return nil, fmt.Errorf("node missing id")
		}
		if ids[n.ID] {
			return nil, fmt.Errorf("duplicate node id %q", n.ID)
		}
		if !known[n.Agent] {
			return nil, fmt.Errorf("unknown agent %q for node %q", n.Agent, n.ID)
		}
		if len(n.Checks) > 0 {
			if err := validateChecks(n.Checks, checkCommands); err != nil {
				return nil, fmt.Errorf("node %q: %w", n.ID, err)
			}
		}
		ids[n.ID] = true
		plan.Nodes = append(plan.Nodes, Node{
			ID:        n.ID,
			AgentName: n.Agent,
			Task:      n.Task,
			Rubric:    n.Rubric,
			DependsOn: n.DependsOn,
			Checks:    n.Checks,
			Workdir:   n.Workdir,
		})
	}

	// Harden: a synthesizer depends on every non-synthesizer node that is NOT
	// DOWNSTREAM OF IT. The orchestrator frequently omits some predecessors, which
	// would let the synthesizer run before research finishes; fill the set in.
	//
	// The "not downstream of it" part is load-bearing. A synthesizer is not always
	// the terminal fan-in: research → synthesize → implement is a perfectly good
	// plan, and there the implementer depends ON the synthesizer. Blindly giving the
	// synthesizer an edge to EVERY other node then points it at its own descendant
	// and manufactures a cycle — which quack rejected as "dag plan contains a cycle",
	// blaming the orchestrator for a correct plan we had just corrupted (live:
	// 5 explorers → synthesize-design → implement-code-mode, rejected repeatedly).
	if len(plan.Nodes) > 1 {
		hasSynth := false
		for _, n := range plan.Nodes {
			if n.AgentName == "synthesizer" {
				hasSynth = true
				break
			}
		}
		for i, n := range plan.Nodes {
			if n.AgentName != "synthesizer" {
				continue
			}
			down := descendants(plan.Nodes, n.ID)
			var deps []string
			for _, m := range plan.Nodes {
				if m.ID == n.ID || m.AgentName == "synthesizer" || down[m.ID] {
					continue
				}
				deps = append(deps, m.ID)
			}
			plan.Nodes[i].DependsOn = deps
		}
		// Harden: a multi-node plan with NO synthesizer and ≥2 terminal nodes
		// can't run as a native graph (single-terminal rule — nativegraph.go).
		// The orchestrator sometimes omits the fan-in entirely; append one
		// rather than failing the whole run. Skipped when the roster has no
		// synthesizer (the graph build will then reject multi-terminal plans).
		if !hasSynth && known["synthesizer"] && len(terminalIDs(plan.Nodes)) > 1 {
			// Safe to depend on every existing node: this fan-in is APPENDED, so
			// nothing depends on it and it can have no descendants to cycle into.
			var all []string
			for _, n := range plan.Nodes {
				all = append(all, n.ID)
			}
			plan.Nodes = append(plan.Nodes, Node{
				ID:        "synthesize",
				AgentName: "synthesizer",
				Task:      "Combine the findings from every preceding node into one complete, well-cited answer to the user's request.",
				DependsOn: all,
			})
		}
	}

	if _, err := topoLayers(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// validateChecks enforces §4's plan-time rule: every check must PREFIX-MATCH a
// configured workspace.check_commands entry (the planner fills in arguments to
// an operator-approved prefix; it never invents an executable command) and
// contain no shell metacharacters (checks run shell-less — pipes are native
// via workspace.RunPipeline; & ; $ < > ` ( ) stay unexpressible, see
// internal/workspace.ContainsShellMetachar). An empty checkCommands (the
// default) means checks are unavailable at all — a plan node that sets them
// is rejected with a targeted, fixable error rather than silently dropped or
// run unchecked.
func validateChecks(checks, checkCommands []string) error {
	if len(checkCommands) == 0 {
		return fmt.Errorf("checks are unavailable (workspace.check_commands is empty) — omit `checks`")
	}
	for _, c := range checks {
		c = strings.TrimSpace(c)
		if c == "" {
			return fmt.Errorf("empty check command")
		}
		if workspace.ContainsShellMetachar(c) {
			return fmt.Errorf("check %q contains a shell metacharacter (& ; $ < > ` ( )) — checks never invoke a shell; pipes are supported natively, the rest is unavailable", c)
		}
		if !workspace.MatchesCheckPrefix(c, checkCommands) {
			return fmt.Errorf("check %q does not match any configured workspace.check_commands prefix (%s)",
				c, strings.Join(checkCommands, ", "))
		}
	}
	return nil
}

// descendants returns every node that transitively DEPENDS ON id — the nodes
// downstream of it. A hardening pass must never add an edge from a node to one of
// its own descendants: that is a cycle by construction.
func descendants(nodes []Node, id string) map[string]bool {
	dependents := map[string][]string{} // dep -> nodes that depend on it
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	out := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range dependents[cur] {
			if !out[next] {
				out[next] = true
				stack = append(stack, next)
			}
		}
	}
	return out
}
