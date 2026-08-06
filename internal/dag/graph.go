package dag

import (
	"errors"
	"fmt"
	"log/slog"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/vetting"
)

const (
	gateFailedKey = "quack.gate_failed/"
	gateScoreKey  = "quack.gate_score/"
	gatePassedKey = "quack.gate_passed/"
	gateRoundsKey = "quack.gate_rounds/"
)

// nodeScopedWorker: fresh worker/model/tools per DAG node.
type nodeScopedWorker interface {
	ForNode(nodeKey string) (worker adkagent.Agent, m model.LLM, tools []tool.Tool, release func(), err error)
}

// buildGateNodes: one gated node per plan node.
func buildGateNodes(plan Plan, agents map[string]adkagent.Agent, models map[string]model.LLM, toolsByAgent map[string][]tool.Tool, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string, recordGate func(nodeID string, score float64, passed bool, rounds int)) (map[string]workflow.Node, []adkagent.Agent, error) {
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
			subAgents = append(subAgents, ag)
		}
		worker := ag
		workerModel := models[n.AgentName]
		workerTools := toolsByAgent[n.AgentName]
		var release func()
		if scoped, ok := ag.(nodeScopedWorker); ok {
			w, m, wt, rel, err := scoped.ForNode(plan.ID + ":" + n.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("dag: node %q: per-node agent construction: %w", n.ID, err)
			}
			worker, workerModel, workerTools, release = w, m, wt, rel
		}
		workerNode, err := vetting.NewWorkerNode(worker)
		if err != nil {
			return nil, nil, err
		}
		node := n
		cfg := nodeGateConfig(plan, node, worker, cfgFor, chatID)
		nodesByID[node.ID] = newGatedNode(plan, node, workerNode, workerModel, workerTools, judge, cfg, mediaAgents, controls, chatID, recordGate, release)
	}
	return nodesByID, subAgents, nil
}

// nodeGateConfig assembles one node's trust-gate config from the agent's base
// config (cfgFor) plus the node's and plan's own facts. A planOnly plan
// forces every node read-only with no delivery target here, in the one place
// cfgFor's result is turned into a node's actual config - regardless of which
// agent the planner picked (#739). Filtering by agent name would miss a
// future writable agent; this keys on the capability fields themselves.
func nodeGateConfig(plan Plan, node Node, worker adkagent.Agent, cfgFor func(string) vetting.Config, chatID string) vetting.Config {
	cfg := cfgFor(node.AgentName)
	cfg.DeliverPromptEvent = vetting.PromptEventNeeded(worker)
	cfg.Checks = node.Checks
	cfg.Workdir = node.Workdir
	cfg.NodeID = workspaceNodeID(plan, node)
	cfg.Agent = node.AgentName
	cfg.Grant = plan.Grant
	// code-reviewer nodes are always review-delivery nodes by construction.
	cfg.IsReviewer = node.AgentName == reviewerAgent
	cfg.Task = node.Task
	cfg.DeriveChecks = node.AgentName == implementerAgent
	cfg.ChatID = chatID
	if plan.Setup != nil && setupQualifyingAgent(node.AgentName) {
		cfg.Setup = &vetting.SetupBranch{Repo: plan.Setup.Repo, WorkBranch: plan.Setup.WorkBranch}
		cfg.ExistingPR = plan.Setup.CheckoutExistingHead
		if nonTerminalRepoChainNode(plan, node) {
			cfg.Deliver = nil
		}
	}
	if plan.PlanOnly {
		cfg.ReadOnly = true
		cfg.Deliver = nil
	}
	return cfg
}

// newGatedNode: assembles worker prompt, runs trust-gate refine loop.
func newGatedNode(plan Plan, node Node, workerNode workflow.Node, workerModel model.LLM, workerTools []tool.Tool, judge vetting.JudgeFactory, cfg vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string, recordGate func(nodeID string, score float64, passed bool, rounds int), release func()) workflow.Node {
	return workflow.NewDynamicNode[any, string](node.ID,
		func(ctx adkagent.Context, in any, emit func(*session.Event) error) (string, error) {
			if release != nil {
				defer release()
			}
			var ctrl vetting.NodeControl
			effectiveNode := node
			if controls != nil {
				nc, override, ok := controls.registerAndTakeOverride(chatID, node.ID)
				defer controls.unregister(chatID, node.ID)
				ctrl = nc
				if ok {
					effectiveNode.Task = override
				}
			}
			cfg.Task = effectiveNode.Task

			upstream := upstreamFromInput(in, node.DependsOn)
			gateFailed := readGateFailed(ctx, node.DependsOn)
			prompt := buildTask(plan, effectiveNode, upstream, gateFailed)
			token := vetting.AdvisorThreadToken(plan.ID, node.ID)
			task := vetting.AdvisorTask{
				Task: effectiveNode.Task, Rubric: node.Rubric, NodeID: node.ID,
				WorkspaceNodeID: workspaceNodeID(plan, node),
				WorktreeParent:  worktreeParentID(plan, node),
				InvocationID:    ctx.InvocationID(),
			}
			if sess := ctx.Session(); sess != nil {
				task.AppName, task.UserID, task.SessionID = sess.AppName(), sess.UserID(), sess.ID()
			}
			memParticipant := cfg.ExternalWorker && cfg.CommitMemory && cfg.Memory != nil
			reviewNode := cfg.ExternalWorker && cfg.ReadOnly && cfg.IsReviewer
			prNode := cfg.ExternalWorker && !cfg.ReadOnly && cfg.Deliver != nil
			if memParticipant || reviewNode || prNode {
				if secret, serr := vetting.NewMemSecret(); serr != nil {
					slog.Warn("acp MCP secret unavailable; node runs without its memory/review tools",
						"component", "dag", "node", node.ID, "err", serr)
				} else {
					task.MemSecret = secret
					ms := vetting.MemSession{}
					if memParticipant {
						ms.Memory = cfg.Memory
						ms.Scope = vetting.MemoryScope(ctx, cfg, node.ID)
						ms.Staged = &vetting.MemStage{}
					}
					if reviewNode {
						ms.Review = &vetting.ReviewStage{}
					}
					if prNode {
						ms.PRStage = &vetting.PRStage{}
						ms.ExistingPR = cfg.ExistingPR
					}
					vetting.RegisterMemSession(secret, ms)
					defer vetting.UnregisterMemSession(secret)
				}
			}
			vetting.RegisterAdvisorThread(token, task)
			defer vetting.UnregisterAdvisorThread(token)
			prompt = prompt + "\n\n" + vetting.AdvisorThreadMarker(token)
			atts := plan.Attachments
			if !mediaAgents[node.AgentName] {
				atts = nil
			}
			ledger.StampCoords(workerTools, ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent})
			answer, res, err := vetting.RunGatedRefine(ctx, node.ID, workerNode, workerModel, judge, cfg, prompt, atts, ctrl, emit)
			if errors.Is(err, vetting.ErrNodeEmpty) {
				markGateFailed(ctx, node.ID)
				return "", nil
			}
			if errors.Is(err, vetting.ErrNodePaused) {
				markGateFailed(ctx, node.ID)
				return answer, nil
			}
			if err == nil {
				if recordGate != nil {
					recordGate(node.ID, res.Score, res.Passed, res.Rounds)
				}
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

// markGateFailed: flags node with no answer for continue-but-warn.
func markGateFailed(ctx adkagent.Context, nodeID string) {
	if st := ctx.State(); st != nil {
		_ = st.Set(gateFailedKey+nodeID, true)
		_ = st.Set(gatePassedKey+nodeID, false)
	}
}

// readGateFailed: reads each dependency's gate-fail flag from session state.
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

// upstreamFromInput: converts edge input into upstream map for buildTask.
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
