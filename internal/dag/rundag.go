package dag

import (
	"fmt"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/workflow"
)

// runDAG schedules a plan's gate nodes via concurrent RunNode in topological
// layers (≤ maxActive running at once), gathering each node's output for its
// dependents. It runs inside ONE runner (the orchestration node's sub-scheduler),
// so a gate node's empty-pause (ErrNodeInterrupted) propagates up to pause the
// whole run; on resume the orchestration node re-runs, completed nodes replay
// from the RunNode cache, and the paused node resumes with the human's reply.
//
// This replaces BuildWorkflow's edge graph + the executor's separate runner.
func runDAG(ctx adkagent.Context, plan Plan, gateNodes map[string]workflow.Node, maxActive int) (map[string]string, error) {
	if maxActive < 1 {
		maxActive = 1
	}
	layers, err := topoLayers(plan)
	if err != nil {
		return nil, err
	}
	nodeByID := make(map[string]Node, len(plan.Nodes))
	for _, n := range plan.Nodes {
		nodeByID[n.ID] = n
	}

	var mu sync.Mutex
	outputs := map[string]string{}
	for _, layer := range layers {
		sem := make(chan struct{}, maxActive)
		errs := make([]error, len(layer))
		var wg sync.WaitGroup
		for i, nid := range layer {
			wg.Add(1)
			go func(i int, nid string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				// Feed this node its dependencies' outputs (dep ID → text), the same
				// shape upstreamFromInput/buildTask expect from a JoinNode fan-in.
				in := map[string]any{}
				mu.Lock()
				for _, d := range nodeByID[nid].DependsOn {
					in[d] = outputs[d]
				}
				mu.Unlock()

				out, rerr := workflow.RunNode[string](ctx, gateNodes[nid], in)
				if rerr != nil {
					errs[i] = rerr
					return
				}
				mu.Lock()
				outputs[nid] = out
				mu.Unlock()
			}(i, nid)
		}
		wg.Wait()
		// A pause (ErrNodeInterrupted) or hard error on any node ends the walk; the
		// error propagates up so the run pauses/aborts. Resume re-runs from the top.
		for _, e := range errs {
			if e != nil {
				return outputs, e
			}
		}
	}
	return outputs, nil
}

// topoLayers groups nodes into dependency layers (Kahn): layer 0 is the leaves,
// each later layer depends only on earlier ones. Nodes within a layer are
// independent and run concurrently. Errors on an unknown dep or a cycle.
func topoLayers(plan Plan) ([][]string, error) {
	indeg := make(map[string]int, len(plan.Nodes))
	dependents := map[string][]string{}
	known := make(map[string]bool, len(plan.Nodes))
	for _, n := range plan.Nodes {
		known[n.ID] = true
	}
	for _, n := range plan.Nodes {
		indeg[n.ID] = len(n.DependsOn)
		for _, d := range n.DependsOn {
			if !known[d] {
				return nil, fmt.Errorf("dag: node %q depends on unknown node %q", n.ID, d)
			}
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	var layers [][]string
	placed := 0
	cur := []string{}
	for id, d := range indeg {
		if d == 0 {
			cur = append(cur, id)
		}
	}
	for len(cur) > 0 {
		layers = append(layers, cur)
		placed += len(cur)
		var next []string
		for _, id := range cur {
			for _, dep := range dependents[id] {
				indeg[dep]--
				if indeg[dep] == 0 {
					next = append(next, dep)
				}
			}
		}
		cur = next
	}
	if placed != len(plan.Nodes) {
		return nil, fmt.Errorf("dag: dependency cycle among %d nodes", len(plan.Nodes)-placed)
	}
	return layers, nil
}
