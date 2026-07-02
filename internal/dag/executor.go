package dag

import (
	"context"
	"iter"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// nodeAppName is the ADK application name for a DAG run's workflow session.
const nodeAppName = "quack-nodes"

// Executor runs a Plan as an ADK v2 graph workflow (BuildWorkflow): one
// first-class gated-worker node per plan node, fanned out per DependsOn. It is
// the v2 replacement for the legacy TopoSort + semaphore + per-node-runner
// executor — ADK's scheduler owns concurrency, ordering, and (on a durable
// session store) restart-durable completed-node skipping.
type Executor struct {
	sessions    session.Service
	agents      map[string]adkagent.Agent             // agent name → built (plain) agent
	judge       vetting.JudgeFactory                  // independent judge factory
	cfgFor      func(agentName string) vetting.Config // per-agent gate config (rubric override etc.)
	mediaAgents map[string]bool                       // agents accepting image/audio parts (media threading TODO)
}

// NewExecutor returns a graph Executor. agents maps agent name → plain agent
// (no longer pre-wrapped in the gate — the graph wraps each node in the refine
// loop). cfgFor supplies the per-agent trust-gate config.
func NewExecutor(sessions session.Service, agents map[string]adkagent.Agent, judge vetting.JudgeFactory, cfgFor func(string) vetting.Config, mediaAgents map[string]bool) *Executor {
	return &Executor{sessions: sessions, agents: agents, judge: judge, cfgFor: cfgFor, mediaAgents: mediaAgents}
}

// Execute builds the plan's workflow and runs it via a fresh runner, translating
// the workflow's session-event stream into SSE (dag_plan → node_start/node_done)
// and filling nodeOutputs (node ID → vetted answer) for the caller's
// TerminalOutput. Full agent-activity SSE fidelity is Phase 4; this emits the DAG
// structure + per-node lifecycle so the frontend renders progress and the caller
// gets the answer.
func (e *Executor) Execute(ctx context.Context, plan Plan, userID, chatID string, nodeOutputs map[string]string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		root, err := BuildWorkflow(plan, e.agents, e.judge, e.cfgFor)
		if err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}
		r, err := runner.New(runner.Config{AppName: nodeAppName, Agent: root, SessionService: e.sessions, AutoCreateSession: true})
		if err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}
		// The plan tool already emitted dag_plan for this plan_id (and M8 persists
		// it); re-emitting here caused a duplicate insert (dag_plans_pkey). The plan
		// tool owns dag_plan emission — the executor only streams node lifecycle.

		// ponytail: media attachments + per-node History threading are deferred
		// (Phase 4 / follow-up). Leaf nodes assemble their prompt from
		// plan.UserMessage via buildTask, so text research works end-to-end now.
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}

		agentByID := make(map[string]string, len(plan.Nodes))
		for _, n := range plan.Nodes {
			agentByID[n.ID] = n.AgentName
		}
		started := map[string]bool{}
		var lastOutput string

		for ev, rerr := range r.Run(ctx, userID, plan.ID, content, adkagent.RunConfig{}) {
			if rerr != nil {
				yield(stream.Errorf(rerr.Error()), nil)
				return
			}
			if ev == nil {
				continue
			}
			id := nodeIDFromEvent(ev)
			agent, isNode := agentByID[id]
			if isNode && !started[id] {
				started[id] = true
				if !yield(stream.NodeStart(id, agent), nil) {
					return
				}
			}
			if ev.Output != nil {
				if out := outputString(ev.Output); out != "" {
					lastOutput = out
					if isNode {
						nodeOutputs[id] = out
						if !yield(stream.NodeDone(id, stream.NodeDoneData{NodeID: id, Output: out, OutputPreview: preview(out)}), nil) {
							return
						}
					}
				}
			}
		}
		// Fallback: if per-node capture missed the terminal (event path shape), seed
		// the terminal node from the workflow's final output so TerminalOutput works.
		if lastOutput != "" {
			ensureTerminal(plan, nodeOutputs, lastOutput)
		}
	}
}

// buildTask assembles a node's worker prompt: the verbatim user request, each
// dependency's output (prefixed with a ⚠ warning when it failed vetting), then the
// node's own task. A leaf with no upstream is just its task.
func buildTask(plan Plan, node Node, upstream map[string]string, gateFailed map[string]bool) string {
	var sb strings.Builder
	if plan.UserMessage != "" {
		sb.WriteString("User's request (verbatim):\n")
		sb.WriteString(plan.UserMessage)
		sb.WriteString("\n\n---\n\n")
	}
	for _, dep := range node.DependsOn {
		if out, ok := upstream[dep]; ok && strings.TrimSpace(out) != "" {
			if gateFailed[dep] {
				sb.WriteString("⚠ WARNING: the following input FAILED independent quality vetting (unverified claims or missing citations). Treat its claims skeptically and do not present them as verified:\n\n")
			}
			sb.WriteString(out)
			sb.WriteString("\n\n---\n\n")
		}
	}
	if sb.Len() == 0 {
		return node.Task
	}
	sb.WriteString("Your task: ")
	sb.WriteString(node.Task)
	return sb.String()
}

// nodeIDFromEvent extracts the emitting node's name from NodeInfo.Path
// ("<parent>/<child>@<runID>" → "<child>"). Worker/join sub-nodes yield names
// that aren't plan node IDs, so the caller's plan-node lookup filters them out.
func nodeIDFromEvent(ev *session.Event) string {
	if ev.NodeInfo == nil || ev.NodeInfo.Path == "" {
		return ""
	}
	seg := ev.NodeInfo.Path
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if i := strings.Index(seg, "@"); i >= 0 {
		seg = seg[:i]
	}
	return seg
}

func outputString(o any) string {
	if s, ok := o.(string); ok {
		return stream.StripThinking(s)
	}
	return ""
}

func preview(s string) string {
	const n = 250
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ensureTerminal seeds the plan's terminal node (no successors) from fallback
// when per-node capture missed it, so TerminalOutput has an answer.
func ensureTerminal(plan Plan, nodeOutputs map[string]string, fallback string) {
	hasSucc := map[string]bool{}
	for _, n := range plan.Nodes {
		for _, d := range n.DependsOn {
			hasSucc[d] = true
		}
	}
	for _, n := range plan.Nodes {
		if !hasSucc[n.ID] {
			if _, ok := nodeOutputs[n.ID]; !ok {
				nodeOutputs[n.ID] = fallback
			}
			return
		}
	}
}
