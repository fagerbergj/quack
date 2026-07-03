package dag

import (
	"errors"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/fagerbergj/quack/internal/vetting"
)

// Session-state key prefixes a gated node writes under its node ID: gate_failed
// (true when the answer did NOT clear threshold) drives continue-but-warn on
// dependents; gate_score/passed/rounds carry the judge result to Execute's
// node_done (the judge runs isolated, off the workflow stream).
const (
	gateFailedKey = "quack.gate_failed/"
	gateScoreKey  = "quack.gate_score/"
	gatePassedKey = "quack.gate_passed/"
	gateRoundsKey = "quack.gate_rounds/"
)

// buildGateNodes builds one gated-worker node per plan node (node ID → node),
// shared by BuildWorkflow (edge graph) and the single-runner runDAG path. Also
// returns the deduped worker agents (for BuildWorkflow's author resolution;
// runDAG ignores them).
func buildGateNodes(plan Plan, agents map[string]adkagent.Agent, models map[string]model.LLM, advisor adkagent.Agent, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string) (map[string]workflow.Node, []adkagent.Agent, error) {
	nodesByID := make(map[string]workflow.Node, len(plan.Nodes))
	var subAgents []adkagent.Agent
	seenAgent := map[string]bool{}
	for _, n := range plan.Nodes {
		ag, ok := agents[n.AgentName]
		if !ok {
			return nil, nil, fmt.Errorf("dag: no agent %q for node %q", n.AgentName, n.ID)
		}
		if !seenAgent[n.AgentName] {
			seenAgent[n.AgentName] = true
			subAgents = append(subAgents, ag) // dedup: author resolution only
		}
		workerNode, err := vetting.NewWorkerNode(ag)
		if err != nil {
			return nil, nil, err
		}
		// The advisor (formative consult) is the same agent for every node; wrap it
		// per node so concurrent nodes don't share one node instance. nil ⇒ the gate
		// skips the consult (e.g. judge/advisor disabled).
		var advisorNode workflow.Node
		if advisor != nil {
			if advisorNode, err = vetting.NewWorkerNode(advisor); err != nil {
				return nil, nil, err
			}
		}
		node := n // capture per iteration
		nodesByID[node.ID] = newGatedNode(plan, node, workerNode, models[node.AgentName], advisorNode, judge, cfgFor(node.AgentName), mediaAgents, controls, chatID)
	}
	return nodesByID, subAgents, nil
}

// newGatedNode builds the dynamic node for one plan node: it assembles the
// worker prompt from upstream outputs, runs the trust-gate refine loop, and
// FAILS (marks the node) on an empty answer. The
// same node works whether it's scheduled by BuildWorkflow's edges or RunNode'd
// directly by an orchestration node (single-runner path).
func newGatedNode(plan Plan, node Node, workerNode workflow.Node, workerModel model.LLM, advisorNode workflow.Node, judge vetting.JudgeFactory, cfg vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string) workflow.Node {
	return workflow.NewDynamicNode[any, string](node.ID,
		func(ctx adkagent.Context, in any, emit func(*session.Event) error) (string, error) {
			upstream := upstreamFromInput(in, node.DependsOn)
			// Continue-but-warn: a dependency whose vetting failed flags itself in
			// session state; buildTask prefixes a ⚠ warning so this node treats that
			// input skeptically.
			gateFailed := readGateFailed(ctx, node.DependsOn)
			prompt := buildTask(plan, node, upstream, gateFailed)
			// Thread the turn's media parts to a media-capable node's worker
			// (image/audio); text-only nodes get nil (a plain string prompt).
			atts := plan.Attachments
			if !mediaAgents[node.AgentName] {
				atts = nil
			}
			// Register a per-node control so CancelNode/SteerNode can reach THIS
			// node while it runs (cooperative, at gate-stage boundaries). Keep it a
			// nil interface when controls are off — a typed-nil would panic in the
			// gate's ctrl.Cancelled() check.
			var ctrl vetting.NodeControl
			if controls != nil {
				nc := controls.register(chatID, node.ID)
				defer controls.unregister(chatID, node.ID)
				ctrl = nc
			}

			answer, res, err := vetting.RunGatedRefine(ctx, node.ID, workerNode, advisorNode, workerModel, judge, cfg, prompt, atts, ctrl, emit)
			if errors.Is(err, vetting.ErrNodeEmpty) {
				// Empty → the node FAILS. The DAG continues (dependents see the gap via
				// buildTask's ⚠ note) and the empty output drives a loud node_failed. A
				// human can retry the failed node afterward.
				markGateFailed(ctx, node.ID)
				return "", nil
			}
			if err == nil {
				// Persist the gate outcome to session state: gate_failed drives
				// continue-but-warn on dependents; score/passed/rounds let Execute
				// surface the judge result on node_done (the judge runs in its own
				// isolated runner, so its result can't ride the workflow stream).
				st := ctx.State()
				_ = st.Set(gateFailedKey+node.ID, !res.Passed)
				_ = st.Set(gateScoreKey+node.ID, res.Score)
				_ = st.Set(gatePassedKey+node.ID, res.Passed)
				_ = st.Set(gateRoundsKey+node.ID, res.Rounds)
			}
			return answer, err
		},
		workflow.NodeConfig{})
}

// markGateFailed flags a node that produced NO answer (cancelled, steered-still-
// empty, or autonomous continue-but-warn) so its dependents get the continue-but-
// warn treatment (buildTask prefixes a ⚠). The empty output itself drives the loud
// node_failed the DagStream emits for it.
func markGateFailed(ctx adkagent.Context, nodeID string) {
	if st := ctx.State(); st != nil {
		_ = st.Set(gateFailedKey+nodeID, true)
		_ = st.Set(gatePassedKey+nodeID, false)
	}
}

// readGateFailed reconstructs the gateFailed map for buildTask by reading each
// dependency's gate-fail flag from workflow session state.
func readGateFailed(ctx adkagent.Context, dependsOn []string) map[string]bool {
	out := map[string]bool{}
	st := ctx.State()
	if st == nil {
		return out
	}
	for _, dep := range dependsOn {
		if v, err := st.Get(gateFailedKey + dep); err == nil {
			if b, ok := v.(bool); ok && b {
				out[dep] = true
			}
		}
	}
	return out
}

// upstreamFromInput converts a dynamic node's edge input into the upstream map
// (dep node ID → output text) that buildTask expects. A JoinNode fan-in delivers
// map[string]any keyed by predecessor node name (== dep node ID); a single
// predecessor delivers its bare string output; a leaf (from Start) gets nil.
func upstreamFromInput(in any, dependsOn []string) map[string]string {
	upstream := map[string]string{}
	switch v := in.(type) {
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok {
				upstream[k] = s
			}
		}
	case string:
		if len(dependsOn) == 1 && v != "" {
			upstream[dependsOn[0]] = v
		}
	}
	return upstream
}
