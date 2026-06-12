// Package dag implements DAG-based task planning and execution for M3.
// The planner decomposes a user request into a directed acyclic graph of
// specialist-agent tasks; the executor runs them in topological order,
// in parallel where possible, and streams progress events.
package dag

import "fmt"

// HistoryTurn is one prior conversation turn, passed to nodes as native ADK
// session events so the model sees real user/model turns rather than a
// flattened transcript.
type HistoryTurn struct {
	Role string // genai role: "user" or "model"
	Text string
}

// Plan is a DAG of agent tasks for one user request. UserMessage is the user's
// request verbatim and History the prior conversation — both flow to every
// node so specialists see the full context, not just the planner's compressed
// task description.
type Plan struct {
	ID          string
	Nodes       []Node
	Edges       []Edge
	UserMessage string
	History     []HistoryTurn
}

// Node is one task in the plan: the agent to run, what to do, an acceptance
// rubric for the judge, and which other nodes' outputs this node depends on.
type Node struct {
	ID        string
	AgentName string
	Task      string
	Rubric    string
	DependsOn []string // IDs of predecessor nodes
}

// Edge is a dependency between two nodes.
type Edge struct {
	From string
	To   string
}

// TopoSort returns the plan's nodes grouped into layers. Nodes in layer 0
// have no dependencies; nodes in layer N depend only on nodes in layers < N.
// Returns an error if the plan contains a cycle.
func TopoSort(p Plan) ([][]Node, error) {
	nodeMap := make(map[string]Node, len(p.Nodes))
	for _, n := range p.Nodes {
		nodeMap[n.ID] = n
	}

	// Build adjacency and in-degree from DependsOn.
	inDegree := make(map[string]int, len(p.Nodes))
	for _, n := range p.Nodes {
		if _, ok := inDegree[n.ID]; !ok {
			inDegree[n.ID] = 0
		}
		for _, dep := range n.DependsOn {
			if _, ok := nodeMap[dep]; !ok {
				return nil, fmt.Errorf("node %q depends on unknown node %q", n.ID, dep)
			}
			inDegree[n.ID]++
		}
	}

	var layers [][]Node
	remaining := len(p.Nodes)
	for remaining > 0 {
		var layer []Node
		for _, n := range p.Nodes {
			if inDegree[n.ID] == 0 {
				layer = append(layer, n)
			}
		}
		if len(layer) == 0 {
			return nil, fmt.Errorf("dag plan contains a cycle")
		}
		layers = append(layers, layer)
		// Remove processed nodes from in-degree counts.
		processed := make(map[string]bool, len(layer))
		for _, n := range layer {
			processed[n.ID] = true
			inDegree[n.ID] = -1 // mark done
		}
		for _, n := range p.Nodes {
			if inDegree[n.ID] < 0 {
				continue
			}
			for _, dep := range n.DependsOn {
				if processed[dep] {
					inDegree[n.ID]--
				}
			}
		}
		remaining -= len(layer)
	}
	return layers, nil
}
