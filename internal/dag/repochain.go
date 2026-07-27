package dag

import (
	"fmt"

	"github.com/fagerbergj/quack/internal/workspace"
)

// validateRepoChain rejects a plan that declares Setup (a shared clone+branch,
// see runPlanSetup) but whose WRITER nodes (implementer - the only agent that
// still resolves directly into the shared clone, see workspaceNodeID) are not
// totally ordered by depends_on: two such nodes that could run concurrently
// would both write to the ONE shared working tree and corrupt it. A
// read-only qualifying node (reviewer, explorer) never shares that risk - it
// gets its own linked git worktree (see worktreeParentID) - so it needs no
// ordering here regardless of how many run concurrently. A plan with fewer
// than 2 writer nodes is always fine; a plan.Setup == nil plan is untouched -
// each repo-touching node still gets its own independent clone
// (setupQualifyingNodes), so concurrent ones don't share anything to corrupt.
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
// workspace.NodeDir, vetting.Config.NodeID). The plan's WRITER
// (implementerAgent - the only setup-qualifying agent left that mutates the
// shared branch) resolves directly into the ONE shared clone
// (workspace.SharedRepoScope); validateRepoChain guarantees at most one
// writer runs at a time. A read-only qualifying node (reviewer, explorer)
// keeps its own dir (node.ID) here too, exactly like every other node - but
// that dir is provisioned as a linked git worktree off the shared clone, not
// an independent one, when worktreeParentID says so (internal/acp's
// resolveNode is what actually creates it).
func workspaceNodeID(plan Plan, node Node) string {
	if plan.Setup != nil && node.AgentName == implementerAgent {
		return workspace.SharedRepoScope
	}
	return node.ID
}

// readOnlyQualifyingAgent reports whether name is a read-only setup-
// qualifying agent (reviewer, explorer) - the subset of setupQualifyingAgent
// that never mutates the shared clone (see agents/*/agent-card.json's
// acp.read_only) and so gets its own linked worktree instead of sharing the
// implementer's working tree directly.
func readOnlyQualifyingAgent(name string) bool {
	return name == reviewerAgent || name == explorerAgent
}

// worktreeParentID returns the WorkspaceNodeID a read-only qualifying node's
// own directory should be provisioned as a git worktree OF - the plan's one
// shared setup clone (workspace.SharedRepoScope) - or "" when node isn't such
// a node (no plan.Setup, or an agent that isn't reviewer/explorer). Threaded
// onto vetting.AdvisorTask.WorktreeParent (dag/graph.go) so internal/acp's
// resolveNode knows to link a worktree rather than hand the node a bare
// directory.
func worktreeParentID(plan Plan, node Node) string {
	if plan.Setup == nil || !readOnlyQualifyingAgent(node.AgentName) {
		return ""
	}
	return workspace.SharedRepoScope
}

// nonTerminalRepoChainNode reports whether node is a WRITER node (see
// workspaceNodeID) in a plan.Setup chain that is NOT the chain's last writer -
// its branch state isn't final yet, so its delivery must never fire even if
// it stages one (see buildGateNodes, vetting.commitDelivery). A read-only
// qualifying node (reviewer, explorer) is never in this set: its own linked
// worktree is never overwritten by a later chain write, so nothing about its
// delivery goes stale from chain position.
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
