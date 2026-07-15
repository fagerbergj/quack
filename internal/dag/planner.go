package dag

import (
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
}

// NewPlanner returns a Planner over the available agent roster and the
// configured check-command prefixes (workspace.check_commands; may be empty).
func NewPlanner(agents []AgentInfo, checkCommands []string) *Planner {
	return &Planner{agents: agents, checkCommands: checkCommands}
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
// current media — all threaded to every node by the executor. Returns an error
// (no silent fallback) so the orchestrator can fix and re-submit.
func (p *Planner) Build(nodes []RawNode, history []HistoryTurn, message string, attachments []*genai.Part) (*Plan, error) {
	plan, err := assemble(nodes, p.agents, p.checkCommands)
	if err != nil {
		return nil, err
	}
	if err := p.checkImplementationRouting(plan, message); err != nil {
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

// checkImplementationRouting is the deterministic backstop for the (observed,
// non-deterministic) failure where the orchestrator collapses a "implement X and
// open a PR" request into a lone web-researcher analyze node and calls the run
// done after merely describing the repo — the code is never written, committed,
// or pushed. When the request reads as implement-AND-deliver (implementationIntent)
// yet the plan has ZERO code-implementer nodes, the plan is malformed: reject it
// with a targeted, fixable error so the orchestrator re-authors the DAG (its own
// re-plan loop is the retry budget; the plan tool tells it to "fix the nodes and
// call again"). A loud WARN also surfaces every firing for operators.
//
// Only a backstop for the obvious case — the plan-work skill guidance carries the
// rest. The intent heuristic can't catch every phrasing and can't read intent
// across a multi-turn conversation; it is deliberately conservative (verb AND
// delivery term) so a correct research plan is never wrongly rejected.
func (p *Planner) checkImplementationRouting(plan *Plan, message string) error {
	// Meaningful only when the roster actually offers a code-implementer AND the
	// request reads as implement-and-deliver.
	hasImplementer := false
	for _, a := range p.agents {
		if a.Name == implementerAgent {
			hasImplementer = true
			break
		}
	}
	if !hasImplementer || !implementationIntent(message) {
		return nil
	}
	// A plan that deliberately has a code-reviewer node is a REVIEW, not a botched
	// implement plan — the orchestrator chose to critique, not to ship. The
	// implement-intent heuristic misfires here because a PR review's injected
	// context (the PR's own "Add …"/"Fix …" title and description) reads as
	// implement-and-deliver; never force an implementer onto a review.
	for _, n := range plan.Nodes {
		if n.AgentName == reviewerAgent || n.AgentName == implementerAgent {
			return nil
		}
	}
	slog.Warn("plan rejected: implement-and-deliver request has no code-implementer node",
		"component", "planner", "message", message)
	// Be explicit about the ONE edit that fixes this. The first version of this
	// message said only "re-author the plan with a code-implementer node", and a live
	// orchestrator answered it TWICE by adding another RESEARCH node — it read the
	// rejection as "your plan is incomplete" rather than "one specific node is
	// missing". Name the fix; forbid the wrong one.
	return fmt.Errorf("this request asks to implement and deliver code (commit/push/open a PR) "+
		"but the plan has no %s node. The terminal deliverable MUST be a %s node whose task is to "+
		"clone the repo, study its conventions, implement the change with tests, run the repo's "+
		"checks, commit, push a branch, and open the PR — a repo analysis is a feeder step, never "+
		"the deliverable.\n"+
		"TO FIX: keep the nodes you already have and ADD ONE node with agent %q, depending on them, "+
		"as the LAST node. Do NOT add more research/explorer nodes — research is not what is missing.",
		implementerAgent, implementerAgent, implementerAgent)
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

// implementationIntent reports whether message asks for code to be implemented AND
// shipped (committed / pushed / PR'd) — the shape that MUST route to a
// code-implementer node. The vocabulary lives in vetting (delivery.go), shared
// with the deterministic delivery check that holds such a node to its word, so
// routing and gating can never drift apart.
func implementationIntent(message string) bool {
	return vetting.ImplementationIntent(message)
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
