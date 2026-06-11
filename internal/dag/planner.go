package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// AgentInfo describes one available agent to the planner.
type AgentInfo struct {
	Name        string
	Description string
}

// Planner calls an LLM to decompose a user query into a DAG plan.
type Planner struct {
	model  model.LLM
	agents []AgentInfo
}

// NewPlanner returns a Planner that uses the given model and agent roster.
func NewPlanner(m model.LLM, agents []AgentInfo) *Planner {
	return &Planner{model: m, agents: agents}
}

// Plan calls the model to produce a DAG for the given user message.
// On any failure it falls back to a single web-researcher node.
func (p *Planner) Plan(ctx context.Context, message string) (*Plan, error) {
	sysPrompt := p.buildSystemPrompt()
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: "/no_think " + message}},
		}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: sysPrompt}}},
		},
	}

	var sb strings.Builder
	for resp, err := range p.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return p.fallback(message), nil // degrade gracefully
		}
		if resp.Content != nil {
			for _, part := range resp.Content.Parts {
				if !part.Thought && part.Text != "" {
					sb.WriteString(part.Text)
				}
			}
		}
	}

	plan, err := parsePlan(sb.String(), p.agents)
	if err != nil {
		return p.fallback(message), nil // degrade gracefully
	}
	return plan, nil
}

// fallback returns a single-node plan using the first available web-researcher.
func (p *Planner) fallback(message string) *Plan {
	agentName := "web-researcher"
	for _, a := range p.agents {
		if strings.Contains(a.Name, "web-researcher") || strings.Contains(a.Name, "researcher") {
			agentName = a.Name
			break
		}
	}
	return &Plan{
		ID: uuid.NewString(),
		Nodes: []Node{{
			ID:        "n1",
			AgentName: agentName,
			Task:      message,
		}},
	}
}

func (p *Planner) buildSystemPrompt() string {
	var agentList strings.Builder
	for _, a := range p.agents {
		agentList.WriteString(fmt.Sprintf("- %s: %s\n", a.Name, a.Description))
	}

	return fmt.Sprintf(`You are a task decomposition specialist. Decompose the user's query into a minimal DAG of research tasks.

Available agents:
%s
Rules:
1. Use web-researcher nodes for factual research subtasks.
2. Use a synthesizer node ONLY when there are 2+ research nodes to combine.
3. For a simple single-topic query, use ONE web-researcher node (no synthesizer needed).
4. For multi-part queries, use 2–4 web-researcher nodes + ONE synthesizer as the final node.
5. Maximum 5 nodes total.
6. Give each node a focused, specific task description.
7. synthesizer depends_on ALL web-researcher nodes.
8. CRITICAL — serial vs parallel researchers:
   - Run researchers IN PARALLEL (depends_on: []) only when they are TRULY independent
     and each can answer its sub-question without knowing the other's results.
     Example: "climate in Dublin" and "things to do in Dublin" are independent.
   - Run researchers SERIALLY (depends_on: [prev_id]) when one task requires the
     SPECIFIC OUTPUT of another — e.g., first find which models exist, then look up
     specs for those exact models. The second researcher receives the first's answer
     as context, so it can search for the right things.
   - Ask yourself: "Could a researcher answer this task without seeing the previous
     researcher's output?" If NO, set depends_on.

Output ONLY a JSON object (no markdown, no explanation):
{
  "nodes": [
    {"id": "n1", "agent": "web-researcher", "task": "...", "depends_on": []},
    {"id": "n2", "agent": "web-researcher", "task": "...", "depends_on": ["n1"]},
    {"id": "n3", "agent": "synthesizer", "task": "Combine findings into a comprehensive answer", "depends_on": ["n1","n2"]}
  ]
}`, agentList.String())
}

// rawNode is the JSON shape the planner LLM is asked to emit.
type rawNode struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	Rubric    string   `json:"rubric,omitempty"`
	DependsOn []string `json:"depends_on"`
}

type rawPlan struct {
	Nodes []rawNode `json:"nodes"`
}

func parsePlan(text string, agents []AgentInfo) (*Plan, error) {
	text = extractJSON(text)
	var raw rawPlan
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	if len(raw.Nodes) == 0 {
		return nil, fmt.Errorf("plan has no nodes")
	}

	knownAgents := make(map[string]bool, len(agents))
	for _, a := range agents {
		knownAgents[a.Name] = true
	}

	nodeIDs := make(map[string]bool, len(raw.Nodes))
	plan := &Plan{ID: uuid.NewString()}
	for _, n := range raw.Nodes {
		if n.ID == "" {
			return nil, fmt.Errorf("node missing id")
		}
		if nodeIDs[n.ID] {
			return nil, fmt.Errorf("duplicate node id %q", n.ID)
		}
		if !knownAgents[n.Agent] {
			return nil, fmt.Errorf("unknown agent %q for node %q", n.Agent, n.ID)
		}
		nodeIDs[n.ID] = true
		plan.Nodes = append(plan.Nodes, Node{
			ID:        n.ID,
			AgentName: n.Agent,
			Task:      n.Task,
			Rubric:    n.Rubric,
			DependsOn: n.DependsOn,
		})
	}

	// Harden: every synthesizer node must depend on ALL non-synthesizer nodes.
	// LLMs frequently omit some predecessors, causing the synthesizer to miss
	// research output. We compute the full transitive closure of non-synthesizer
	// nodes and replace the synthesizer's depends_on with that complete set.
	// This preserves serial researcher chains: if n2 depends_on n1, the synthesizer
	// still lists both, which is redundant but harmless (TopoSort handles it).
	if len(plan.Nodes) > 1 {
		var nonSynthIDs []string
		for _, n := range plan.Nodes {
			if n.AgentName != "synthesizer" {
				nonSynthIDs = append(nonSynthIDs, n.ID)
			}
		}
		for i, n := range plan.Nodes {
			if n.AgentName == "synthesizer" {
				plan.Nodes[i].DependsOn = nonSynthIDs
			}
		}
	}

	// Build edges from the (possibly corrected) DependsOn arrays.
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			plan.Edges = append(plan.Edges, Edge{From: dep, To: n.ID})
		}
	}

	// Validate: must be acyclic.
	if _, err := TopoSort(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// extractJSON finds the outermost {...} JSON object in text (strips markdown fences).
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	// Strip markdown code fences.
	for _, prefix := range []string{"```json", "```"} {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimPrefix(text, prefix)
			text = strings.TrimSuffix(strings.TrimSpace(text), "```")
			return strings.TrimSpace(text)
		}
	}
	// Find the first '{' and last '}'.
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}
