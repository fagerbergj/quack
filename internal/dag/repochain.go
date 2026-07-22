package dag

import (
	"fmt"

	"github.com/fagerbergj/quack/internal/workspace"
)

// validateRepoChain rejects a plan that declares Setup (a shared clone+branch,
// see runPlanSetup) but whose repo-touching nodes (implementer/reviewer) are
// not totally ordered by depends_on: two such nodes that could run
// concurrently would both write to the ONE shared working tree and corrupt
// it. A plan with fewer than 2 repo-touching nodes is always fine; a
// plan.Setup == nil plan is untouched - each repo-touching node still gets
// its own independent clone (setupQualifyingNodes), so concurrent ones don't
// share anything to corrupt.
func validateRepoChain(plan Plan) error {
	if plan.Setup == nil {
		return nil
	}
	var repoNodes []Node
	for _, n := range plan.Nodes {
		if n.AgentName == implementerAgent || n.AgentName == reviewerAgent {
			repoNodes = append(repoNodes, n)
		}
	}
	for i, a := range repoNodes {
		anc := ancestors(plan.Nodes, a.ID)
		for _, b := range repoNodes[i+1:] {
			if anc[b.ID] || ancestors(plan.Nodes, b.ID)[a.ID] {
				continue // one transitively depends on the other - ordered
			}
			return fmt.Errorf("dag: plan declares setup (a shared clone+branch) but repo-touching nodes %q and %q are not chained by depends_on - concurrent repo-touching nodes would corrupt the one shared working tree; make one depend (directly or transitively) on the other, or drop one",
				a.ID, b.ID)
		}
	}
	return nil
}

// ancestors returns every node id that id transitively DEPENDS ON - the nodes
// upstream of it. Sibling of descendants (planner.go), walking DependsOn
// instead of the reverse (dependents) edge. Assumes an acyclic plan (callers
// run this after topoLayers's cycle check).
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

// workspaceNodeID returns the workspace-relative "node" identifier a plan
// node's fs/git tools and deterministic checks resolve into (see
// workspace.NodeDir, vetting.Config.NodeID). A repo-touching node
// (implementer/reviewer) in a plan.Setup chain resolves into the ONE shared
// clone (workspace.SharedRepoScope) every other repo-touching node in the
// chain also resolves into - validateRepoChain guarantees the chain runs one
// node at a time, so sharing the directory is safe. Every other node keeps
// its own dir (node.ID), unchanged from before this existed.
func workspaceNodeID(plan Plan, node Node) string {
	if plan.Setup != nil && (node.AgentName == implementerAgent || node.AgentName == reviewerAgent) {
		return workspace.SharedRepoScope
	}
	return node.ID
}

// nonTerminalRepoChainNode reports whether node is a repo-touching node in a
// plan.Setup chain that is NOT the chain's last node - its branch state isn't
// final yet, so its delivery must never fire even if it stages one (see
// buildGateNodes, vetting.commitDelivery).
func nonTerminalRepoChainNode(plan Plan, node Node) bool {
	if plan.Setup == nil || (node.AgentName != implementerAgent && node.AgentName != reviewerAgent) {
		return false
	}
	for _, n := range plan.Nodes {
		if n.ID == node.ID || (n.AgentName != implementerAgent && n.AgentName != reviewerAgent) {
			continue
		}
		if ancestors(plan.Nodes, n.ID)[node.ID] {
			return true // n (also repo-touching) depends on node - node isn't terminal
		}
	}
	return false
}
