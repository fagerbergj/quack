package dag

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/workflow"
)

// runDAGSubset runs retry-set nodes with seeded outputs for the rest. maxActive
// is a host-resource ceiling (jail/clone CPU+RAM), not the GPU limiter - each
// gated node's Admission.Admit call (#1007) is the real one.
// ponytail: migrate to native graph if ADK grows per-node seeding.
func runDAGSubset(ctx adkagent.Context, plan Plan, gateNodes map[string]workflow.Node, maxActive int, seeded map[string]string, run map[string]bool) (map[string]string, error) {
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
	outputs := make(map[string]string, len(plan.Nodes))
	for k, v := range seeded {
		outputs[k] = v // reused outputs for nodes not being re-run
	}
	for _, layer := range layers {
		var todo []string
		for _, nid := range layer {
			if run[nid] {
				todo = append(todo, nid)
			}
		}
		if len(todo) == 0 {
			continue
		}
		sem := make(chan struct{}, maxActive)
		errs := make([]error, len(todo))
		var wg sync.WaitGroup
		for i, nid := range todo {
			wg.Add(1)
			go func(i int, nid string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				// Feed this node its dependencies' outputs (dep ID → text) - a mix of
				// freshly-run and seeded - the shape upstreamFromInput/buildTask expect.
				in := map[string]any{}
				mu.Lock()
				for _, d := range nodeByID[nid].DependsOn {
					in[d] = outputs[d]
				}
				mu.Unlock()

				// WithUseSubBranch keeps concurrently re-run nodes' events branch-distinguishable.
				out, rerr := workflow.RunNode[string](ctx, gateNodes[nid], in, workflow.WithUseSubBranch())
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
		for _, e := range errs {
			if e != nil {
				return outputs, e
			}
		}
	}
	return outputs, nil
}

// retrySet returns nodeID plus every node that transitively depends on it.
func retrySet(plan Plan, nodeID string) map[string]bool {
	dependents := map[string][]string{}
	for _, n := range plan.Nodes {
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	set := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if set[id] {
			return
		}
		set[id] = true
		for _, dep := range dependents[id] {
			walk(dep)
		}
	}
	walk(nodeID)
	return set
}

// topoLayers groups nodes into dependency layers (Kahn).
func topoLayers(plan Plan) ([][]string, error) {
	indeg := make(map[string]int, len(plan.Nodes))
	dependents := map[string][]string{}
	known := make(map[string]bool, len(plan.Nodes))
	ids := make([]string, 0, len(plan.Nodes))
	for _, n := range plan.Nodes {
		known[n.ID] = true
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	for _, n := range plan.Nodes {
		indeg[n.ID] = len(n.DependsOn)
		for _, d := range n.DependsOn {
			if !known[d] {
				return nil, fmt.Errorf("dag: node %q depends on unknown node %q; declared node ids: %s",
					n.ID, d, strings.Join(ids, ", "))
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
		var stuck []string
		for id, d := range indeg {
			if d > 0 {
				stuck = append(stuck, id)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("dag: dependency cycle among nodes: %s", strings.Join(stuck, ", "))
	}
	return layers, nil
}
