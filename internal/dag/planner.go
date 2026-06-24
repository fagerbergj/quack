package dag

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/genai"
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
}

// NewPlanner returns a Planner over the available agent roster.
func NewPlanner(agents []AgentInfo) *Planner { return &Planner{agents: agents} }

// RawNode is one DAG node the orchestrator submits to the plan tool.
type RawNode struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	Rubric    string   `json:"rubric,omitempty"`
	DependsOn []string `json:"depends_on"`
}

// Build validates the submitted nodes into a Plan and stamps the turn context.
// message is the verbatim user request, history the prior turns, attachments the
// current media — all threaded to every node by the executor. Returns an error
// (no silent fallback) so the orchestrator can fix and re-submit.
func (p *Planner) Build(nodes []RawNode, history []HistoryTurn, message string, attachments []*genai.Part) (*Plan, error) {
	plan, err := assemble(nodes, p.agents)
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
func assemble(nodes []RawNode, agents []AgentInfo) (*Plan, error) {
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
		ids[n.ID] = true
		plan.Nodes = append(plan.Nodes, Node{
			ID:        n.ID,
			AgentName: n.Agent,
			Task:      n.Task,
			Rubric:    n.Rubric,
			DependsOn: n.DependsOn,
		})
	}

	// Harden: every synthesizer node depends on ALL non-synthesizer nodes. The
	// orchestrator frequently omits some predecessors, which would let the
	// synthesizer run before research finishes; replace its depends_on with the
	// complete set (redundant serial edges are harmless — TopoSort dedups).
	if len(plan.Nodes) > 1 {
		var nonSynth []string
		for _, n := range plan.Nodes {
			if n.AgentName != "synthesizer" {
				nonSynth = append(nonSynth, n.ID)
			}
		}
		for i, n := range plan.Nodes {
			if n.AgentName == "synthesizer" {
				plan.Nodes[i].DependsOn = nonSynth
			}
		}
	}

	if _, err := TopoSort(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}
