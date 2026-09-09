package dag

import (
	"fmt"

	"github.com/fagerbergj/quack/internal/workspace"
)

func validateRepoChain(plan Plan) error {
	if plan.Setup == nil {
		return nil
	}
	var writers []Node
	for _, n := range plan.Nodes {
		if n.AgentName == implementerAgent {
			writers = append(writers, n)
		}
	}
	for i, a := range writers {
		anc := ancestors(plan.Nodes, a.ID)
		for _, b := range writers[i+1:] {
			if anc[b.ID] || ancestors(plan.Nodes, b.ID)[a.ID] {
				continue // one transitively depends on the other - ordered
			}
			return fmt.Errorf("dag: plan declares setup (a shared clone+branch) but writer nodes %q and %q are not chained by depends_on - concurrent writers would corrupt the one shared working tree; make one depend (directly or transitively) on the other, or drop one",
				a.ID, b.ID)
		}
	}
	return nil
}

func ancestors(nodes []Node, id string) map[string]bool {
	byID := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	out := map[string]bool{}
	stack := append([]string{}, byID[id].DependsOn...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		stack = append(stack, byID[cur].DependsOn...)
	}
	return out
}

// workspaceNodeID: workspace scope a node's fs/git tools resolve into.
func workspaceNodeID(plan Plan, node Node) string {
	if plan.Setup != nil && node.AgentName == implementerAgent {
		return workspace.SharedRepoScope
	}
	return node.ID
}

func readOnlyQualifyingAgent(name string) bool {
	return name == reviewerAgent || name == explorerAgent
}

func worktreeParentID(plan Plan, node Node) string {
	if plan.Setup == nil || !readOnlyQualifyingAgent(node.AgentName) {
		return ""
	}
	return workspace.SharedRepoScope
}

func nonTerminalRepoChainNode(plan Plan, node Node) bool {
	if plan.Setup == nil || node.AgentName != implementerAgent {
		return false
	}
	for _, n := range plan.Nodes {
		if n.ID == node.ID || n.AgentName != implementerAgent {
			continue
		}
		if ancestors(plan.Nodes, n.ID)[node.ID] {
			return true // n (also a writer) depends on node - node isn't terminal
		}
	}
	return false
}
