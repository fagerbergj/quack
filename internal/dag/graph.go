package dag

import (
	"errors"
	"fmt"
	"log/slog"

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
func buildGateNodes(plan Plan, agents map[string]adkagent.Agent, models map[string]model.LLM, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string, recordGate func(nodeID string, score float64, passed bool, rounds int)) (map[string]workflow.Node, []adkagent.Agent, error) {
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
		// Per-node CLIENT identity for an A2A agent (agent.nodeClient.ForNode):
		// concurrent nodes running the SAME agent share ONE workflow session, and
		// remoteagent picks the remote A2A session to continue by scanning that
		// session backward for an event authored by its own Name — so with the
		// plain agent name a node adopts a SIBLING's remote session, task and all
		// (live: the OpenHands explorer cloned goose). Keyed by plan+node so it is
		// unique across nodes yet stable across a node's judge/revise rounds and a
		// HITL resume (the same remote session must be resumed there). Local
		// (non-A2A) agents don't implement ForNode and are used as-is.
		worker := ag
		if scoped, ok := ag.(interface {
			ForNode(string) (adkagent.Agent, error)
		}); ok {
			w, err := scoped.ForNode(plan.ID + ":" + n.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("dag: node %q: per-node a2a client: %w", n.ID, err)
			}
			worker = w
		}
		workerNode, err := vetting.NewWorkerNode(worker)
		if err != nil {
			return nil, nil, err
		}
		// cfgFor returns a fresh Config copy (map-of-struct lookup) — safe to
		// stamp this node's own fields onto it without affecting other nodes
		// sharing the same agent.
		cfg := cfgFor(n.AgentName)
		// Remote (A2A) workers never see RunNode input — the gate must deliver
		// each prompt as a session event instead (see vetting.PromptEventNeeded).
		cfg.DeliverPromptEvent = vetting.PromptEventNeeded(worker)
		node := n // capture per iteration
		// Orchestrator-set deterministic gate checks (§4): the deterministic
		// stage (vetting.checksPassCriterion) reads these off Config, not off
		// the plan node directly, so it stays plan/executor-agnostic.
		cfg.Checks = node.Checks
		cfg.Workdir = node.Workdir
		// The workspace-directory scope for this node's fs/git tools and
		// deterministic checks — node.ID for almost every node, but the ONE
		// shared clone (workspace.SharedRepoScope) for a repo-touching node
		// sharing a plan.Setup chain (see workspaceNodeID).
		cfg.NodeID = workspaceNodeID(plan, node)
		// Carried only for observability (span/metric attribute) — see vetting.Config.Agent.
		cfg.Agent = node.AgentName
		// The structural review-delivery signal (#482): a code-reviewer node IS a
		// review-delivery node by construction, independent of how its task is
		// worded — see vetting.Config.IsReviewer.
		cfg.IsReviewer = node.AgentName == reviewerAgent
		// The node's task text drives the deterministic delivery check
		// (vetting/delivery.go): a task that says commit/push/open-a-PR cannot pass
		// unless the workspace ledger shows the worker actually did it.
		cfg.Task = node.Task
		// The planner has NOT seen the repo when it authors the plan, so `checks`
		// are optional: for a code-implementer node the gate DERIVES them from the
		// cloned repo itself (vetting.deriveChecks) when the planner set none. A
		// planner-set list still wins.
		cfg.DeriveChecks = node.AgentName == implementerAgent
		// Per-chat workspace scope: the node's deterministic checks resolve their
		// Workdir under <root>/<user>/<chatID>/ — the same tree the node's own
		// git_clone/fs tools wrote to (they derive the SAME chatID from the
		// advisor-thread marker; see internal/tools chatScopeFromContext). chatID
		// here is the run's chat/session id (== the workflow session id).
		cfg.ChatID = chatID
		// Thread the plan's declared Setup to the gate/delivery (see
		// vetting.Config.Setup) — ONLY for the nodes runPlanSetup actually
		// provisioned (setupQualifyingNodes: implementer/reviewer), the same
		// set commitDelivery's workspace.SetupCloneDir(node.ID) resolves
		// against.
		if plan.Setup != nil && (node.AgentName == implementerAgent || node.AgentName == reviewerAgent) {
			cfg.Setup = &vetting.SetupBranch{Repo: plan.Setup.Repo, WorkBranch: plan.Setup.WorkBranch}
			if nonTerminalRepoChainNode(plan, node) {
				// Delivery fires exactly once, at the chain's terminal node — the
				// only point the shared branch is complete. A mid-chain node that
				// stages a PR anyway must never have it posted.
				cfg.Deliver = nil
			}
		}
		nodesByID[node.ID] = newGatedNode(plan, node, workerNode, models[node.AgentName], judge, cfg, mediaAgents, controls, chatID, recordGate)
	}
	return nodesByID, subAgents, nil
}

// newGatedNode builds the dynamic node for one plan node: it assembles the
// worker prompt from upstream outputs, runs the trust-gate refine loop, and
// FAILS (marks the node) on an empty answer. The
// same node works whether it's scheduled by BuildWorkflow's edges or RunNode'd
// directly by an orchestration node (single-runner path).
func newGatedNode(plan Plan, node Node, workerNode workflow.Node, workerModel model.LLM, judge vetting.JudgeFactory, cfg vetting.Config, mediaAgents map[string]bool, controls *runControls, chatID string, recordGate func(nodeID string, score float64, passed bool, rounds int)) workflow.Node {
	return workflow.NewDynamicNode[any, string](node.ID,
		func(ctx adkagent.Context, in any, emit func(*session.Event) error) (string, error) {
			// Register a per-node control so CancelNode/PauseNode/QueueNodeMessage
			// can reach THIS node while it runs (cooperative, at gate-stage
			// boundaries), and atomically pick up any pending prompt edit for this
			// not-yet-started node (SetNodeTaskOverride) in the SAME critical
			// section as the registration — see runControls.registerAndTakeOverride.
			// Keep ctrl a nil interface when controls are off — a typed-nil would
			// panic in the gate's ctrl.Cancelled() check. effectiveNode carries the
			// overridden task text (or node.Task unchanged) everywhere a node body
			// would otherwise read node.Task: prompt construction, the advisor
			// task, and cfg.Task (the deterministic delivery check).
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
			// Continue-but-warn: a dependency whose vetting failed flags itself in
			// session state; buildTask prefixes a ⚠ warning so this node treats that
			// input skeptically.
			gateFailed := readGateFailed(ctx, node.DependsOn)
			prompt := buildTask(plan, effectiveNode, upstream, gateFailed)
			// Advisor-thread identity: stamp a per-node marker line into the worker's
			// prompt — the ONE channel that reaches the ask_advisor tool even across
			// the A2A hop (the tool executes in the A2A server's runner, where the
			// calling node is otherwise invisible) — and register the node's task +
			// rubric under the token so the tool seeds the mentor's session with the
			// desired outcome on first consult. Trailing placement + last-match
			// parsing keeps the token unambiguous even when foreign markers ride
			// along (see vetting.AdvisorThreadMarker). Re-entering the node (HITL
			// resume, retry) re-registers, so the deferred unregister can't strand a
			// running consult. See vetting/advisor_thread.go.
			token := vetting.AdvisorThreadToken(plan.ID, node.ID)
			// The registration also carries THIS workflow session's coordinates +
			// invocation so guard-ladder tools (internal/tools/guard.go) can scan
			// the workflow session for confirm decisions even when they execute
			// inside the A2A server's runner (whose own context session holds no
			// gate events — see vetting.AdvisorTask).
			task := vetting.AdvisorTask{
				Task: effectiveNode.Task, Rubric: node.Rubric, NodeID: node.ID,
				// WorkspaceNodeID, not NodeID, is what the fs/git tools resolve their
				// directory scope from (internal/tools scopeFromContext) — NodeID
				// itself stays the REAL node id for cancel/pause/queue lookups
				// (controls are registered under it above), which must never be
				// redirected to the shared scope.
				WorkspaceNodeID: workspaceNodeID(plan, node),
				InvocationID:    ctx.InvocationID(),
			}
			if sess := ctx.Session(); sess != nil {
				task.AppName, task.UserID, task.SessionID = sess.AppName(), sess.UserID(), sess.ID()
			}
			// An external worker gets ONE per-node MCP surface (internal/acp) whose
			// credential is a FRESH random secret — never the advisor-thread token
			// above, which is derivable (planID+nodeID) and disclosed to sibling
			// nodes via the prompt, so it must never double as a bearer credential
			// handed to an untrusted external subprocess (see AdvisorTask.MemSecret).
			// The session carries whichever tool buffers the node is entitled to:
			//   - memory (load_memory/stage_memory, #344) for a memory participant;
			//   - review (stage_review_comment/stage_review, #451) for a code-reviewer
			//     node (structural — see vetting.Config.IsReviewer, #482);
			//   - stage_pr for a WRITE worker at the terminal delivery node.
			// Native agents have ADK-native equivalents, so both ride cfg.ExternalWorker.
			memParticipant := cfg.ExternalWorker && cfg.CommitMemory && cfg.Memory != nil
			reviewNode := cfg.ExternalWorker && cfg.ReadOnly && cfg.IsReviewer
			// A WRITE worker at the chain's terminal delivery point (cfg.Deliver
			// non-nil, mid-chain nodes get nil at graph.go:113) gets stage_pr —
			// the same structural gate augmentFromRepo stages its fallback PR under.
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
					}
					vetting.RegisterMemSession(secret, ms)
					// Backstop: RunGatedRefine unregisters as soon as it drains the
					// staging buffer, but several of its early-return paths (empty
					// answer, cancelled, judge error) skip that point entirely — this
					// defer guarantees the session never outlives the node regardless.
					defer vetting.UnregisterMemSession(secret)
				}
			}
			vetting.RegisterAdvisorThread(token, task)
			defer vetting.UnregisterAdvisorThread(token)
			prompt = prompt + "\n\n" + vetting.AdvisorThreadMarker(token)
			// Thread the turn's media parts to a media-capable node's worker
			// (image/audio); text-only nodes get nil (a plain string prompt).
			atts := plan.Attachments
			if !mediaAgents[node.AgentName] {
				atts = nil
			}
			answer, res, err := vetting.RunGatedRefine(ctx, node.ID, workerNode, workerModel, judge, cfg, prompt, atts, ctrl, emit)
			if errors.Is(err, vetting.ErrNodeEmpty) {
				// Empty → the node FAILS. The DAG continues (dependents see the gap via
				// buildTask's ⚠ note) and the empty output drives a loud node_failed. A
				// human can retry the failed node afterward.
				markGateFailed(ctx, node.ID)
				return "", nil
			}
			if errors.Is(err, vetting.ErrNodePaused) {
				// Paused → same graph-completion constraint as cancel/empty (the
				// static workflow graph needs this node to return to unblock its
				// dependents — see dag.Executor.PauseNode's ponytail note): the DAG
				// continues on the partial answer (continue-but-warn), and the
				// stream layer (executor.go's userPaused check) renders it as
				// node_paused, resumable, instead of node_done/node_cancelled.
				markGateFailed(ctx, node.ID)
				return answer, nil
			}
			if err == nil {
				// Record the gate outcome IN PROCESS first: node_done is assembled
				// before this node's state delta is appended, so a fresh sessions.Get
				// cannot see the Set()s below.
				if recordGate != nil {
					recordGate(node.ID, res.Score, res.Passed, res.Rounds)
				}
				// Session state too: gate_failed drives continue-but-warn on dependents
				// (read in-process, where the delta IS visible) and it survives a resume.
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
