package dag

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

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
	plan.History = history
	plan.UserMessage = message
	plan.Attachments = attachments
	return plan, nil
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

	// Harden: every synthesizer node depends on ALL non-synthesizer nodes. The
	// orchestrator frequently omits some predecessors, which would let the
	// synthesizer run before research finishes; replace its depends_on with the
	// complete set (redundant serial edges are harmless — TopoSort dedups).
	if len(plan.Nodes) > 1 {
		var nonSynth []string
		hasSynth := false
		for _, n := range plan.Nodes {
			if n.AgentName != "synthesizer" {
				nonSynth = append(nonSynth, n.ID)
			} else {
				hasSynth = true
			}
		}
		for i, n := range plan.Nodes {
			if n.AgentName == "synthesizer" {
				plan.Nodes[i].DependsOn = nonSynth
			}
		}
		// Harden: a multi-node plan with NO synthesizer and ≥2 terminal nodes
		// can't run as a native graph (single-terminal rule — nativegraph.go).
		// The orchestrator sometimes omits the fan-in entirely; append one
		// rather than failing the whole run. Skipped when the roster has no
		// synthesizer (the graph build will then reject multi-terminal plans).
		if !hasSynth && known["synthesizer"] && len(terminalIDs(plan.Nodes)) > 1 {
			plan.Nodes = append(plan.Nodes, Node{
				ID:        "synthesize",
				AgentName: "synthesizer",
				Task:      "Combine the findings from every preceding node into one complete, well-cited answer to the user's request.",
				DependsOn: nonSynth,
			})
		}
	}

	if _, err := TopoSort(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// validateChecks enforces §4's plan-time rule: every check must PREFIX-MATCH a
// configured workspace.check_commands entry (the planner fills in arguments to
// an operator-approved prefix; it never invents an executable command) and
// contain no shell metacharacters (checks run as argv only — no shell, see
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
			return fmt.Errorf("check %q contains a shell metacharacter — checks run as argv only, no shell", c)
		}
		if !matchesCheckPrefix(c, checkCommands) {
			return fmt.Errorf("check %q does not match any configured workspace.check_commands prefix (%s)",
				c, strings.Join(checkCommands, ", "))
		}
	}
	return nil
}

// matchesCheckPrefix reports whether check IS one of prefixes, or extends one
// with a space-separated continuation (e.g. "go test ./..." extends "go
// test"; "go testing" does not).
func matchesCheckPrefix(check string, prefixes []string) bool {
	for _, p := range prefixes {
		if check == p || strings.HasPrefix(check, p+" ") {
			return true
		}
	}
	return false
}
