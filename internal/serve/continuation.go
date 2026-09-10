package serve

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
)

// nodeLookupFromStore adapts store.Store.FindDagNodeByChat to dag.NodeLookup
// - dag cannot import store (store already imports dag), so the executor's
// continue: validation is wired here instead.
func nodeLookupFromStore(st *store.Store) dag.NodeLookup {
	return func(ctx context.Context, chatID, nodeID, excludePlanID string) (dag.PriorNode, bool, error) {
		n, err := st.FindDagNodeByChat(ctx, chatID, nodeID, excludePlanID)
		if err != nil || n == nil {
			return dag.PriorNode{}, false, err
		}
		return dag.PriorNode{
			Status: dag.NodeStatus(n.Status),
			Handle: dag.DecodeSessionHandle(n.SessionHandle),
			Output: n.Output,
		}, true, nil
	}
}

// resumableNodesFromStore adapts store.Store.ListResumableDagNodes to the
// orchestrator's plan-tool candidate list - see Orchestrator.SetResumableNodesLookup.
func resumableNodesFromStore(st *store.Store) func(ctx context.Context, chatID string) ([]dag.ResumableNode, error) {
	return func(ctx context.Context, chatID string) ([]dag.ResumableNode, error) {
		nodes, err := st.ListResumableDagNodes(ctx, chatID)
		if err != nil {
			return nil, err
		}
		out := make([]dag.ResumableNode, len(nodes))
		for i, n := range nodes {
			h := dag.DecodeSessionHandle(n.SessionHandle)
			out[i] = dag.ResumableNode{ID: n.NodeID, Agent: h.Agent, Summary: n.OutputPreview}
		}
		return out, nil
	}
}
