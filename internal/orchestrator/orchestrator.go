// Package orchestrator is Quack's request entrypoint. In M3 it decomposes each
// request into a DAG via the planner, runs the DAG with the executor (specialist
// nodes in topological order, parallel within a layer), and persists the turn to
// the ADK session store so chat history survives the change in architecture.
package orchestrator

import (
	"context"
	"iter"
	"log"
	"strings"

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// AppName is the ADK application name used for the chat-history session
// (the quack namespace, separate from each specialist's own A2A namespace).
const AppName = "quack"

// Orchestrator plans and executes a DAG of specialist-agent tasks for each
// user turn, then persists the user message and final answer to the ADK
// session so store.Messages() continues to work unchanged.
type Orchestrator struct {
	sessions session.Service
	planner  *dag.Planner
	executor *dag.Executor
}

// New builds the orchestrator. model is used for both planning and the
// orchestrator's own title generation (passed separately); clients is the
// full map of A2A agent clients keyed by their agent name.
func New(sessions session.Service, planner *dag.Planner, executor *dag.Executor) *Orchestrator {
	return &Orchestrator{sessions: sessions, planner: planner, executor: executor}
}

// Run decomposes message into a DAG, executes it, and yields SSE events.
// The final answer from the last DAG node is persisted as the assistant turn.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, message string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		// Build the prior-conversation context BEFORE persisting the current
		// message, so the new message isn't duplicated into its own history.
		history := o.buildHistory(ctx, userID, sessionID)

		// Persist the user message immediately — not at the end of the run —
		// so a refresh mid-run or a failed run still shows the user's side of
		// the turn, and turn-row/session-event index pairing never skews.
		invID := o.persistUserMessage(ctx, userID, sessionID, message)

		// Plan the DAG.
		plan, err := o.planner.Plan(ctx, history, message)
		if err != nil {
			yield(stream.Errorf("planner: "+err.Error()), nil)
			return
		}
		log.Printf("dag: plan %s nodes=%d", plan.ID, len(plan.Nodes))

		// Emit the dag_plan event so the frontend can render the DAG skeleton.
		nodes := make([]stream.DagNodeDef, len(plan.Nodes))
		for i, n := range plan.Nodes {
			nodes[i] = stream.DagNodeDef{
				ID:        n.ID,
				Agent:     n.AgentName,
				Task:      n.Task,
				DependsOn: n.DependsOn,
			}
		}
		edges := make([]stream.DagEdgeDef, len(plan.Edges))
		for i, e := range plan.Edges {
			edges[i] = stream.DagEdgeDef{From: e.From, To: e.To}
		}
		if !yield(stream.DagPlan(plan.ID, nodes, edges), nil) {
			return
		}

		// Execute the DAG, collecting each node's final output text.
		nodeOutputs := make(map[string]string)
		for ev, err := range o.executor.Execute(ctx, *plan, userID, nodeOutputs) {
			if err != nil {
				yield(stream.Errorf(err.Error()), nil)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}

		// The last node in topological order is the final answer.
		finalAnswer := lastOutput(plan, nodeOutputs)

		// Persist the final answer to the quack session so store.Messages()
		// sees a proper chat history for this turn.
		o.persistAnswer(ctx, userID, sessionID, invID, plan.Nodes, finalAnswer)
	}
}

// lastOutput returns the output of the terminal node (the node with no
// successors in the plan). Falls back to the last node's output if ambiguous.
func lastOutput(plan *dag.Plan, outputs map[string]string) string {
	hasSuccessor := make(map[string]bool)
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			hasSuccessor[dep] = true
		}
	}
	for _, n := range plan.Nodes {
		if !hasSuccessor[n.ID] {
			if out, ok := outputs[n.ID]; ok {
				return out
			}
		}
	}
	// Fallback: last node in the slice.
	for i := len(plan.Nodes) - 1; i >= 0; i-- {
		if out, ok := outputs[plan.Nodes[i].ID]; ok {
			return out
		}
	}
	return ""
}

// maxHistoryChars caps the conversation context passed to the planner and
// nodes. Oldest turns are dropped first when the transcript exceeds it.
const maxHistoryChars = 24000

// buildHistory renders the prior conversation as alternating "User:"/
// "Assistant:" blocks from the quack session events. Returns "" for a fresh
// chat. Thinking parts and tool traffic are excluded — only the visible
// transcript matters for follow-up context.
func (o *Orchestrator) buildHistory(ctx context.Context, userID, sessionID string) string {
	resp, err := o.sessions.Get(ctx, &session.GetRequest{
		AppName: AppName, UserID: userID, SessionID: sessionID,
	})
	if err != nil || resp == nil {
		return ""
	}

	var blocks []string
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		var text strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && p.Text != "" {
				text.WriteString(p.Text)
			}
		}
		if text.Len() == 0 {
			continue
		}
		role := "Assistant"
		if ev.Author == "user" {
			role = "User"
		}
		blocks = append(blocks, role+": "+text.String())
	}
	if len(blocks) == 0 {
		return ""
	}

	// Keep the most recent turns within budget; drop oldest first.
	total := 0
	start := len(blocks)
	for i := len(blocks) - 1; i >= 0; i-- {
		total += len(blocks[i]) + 2
		if total > maxHistoryChars {
			break
		}
		start = i
	}
	return strings.Join(blocks[start:], "\n\n")
}

// persistUserMessage writes the user message to the "quack" ADK session at the
// start of the run, creating the session if needed. Returns the invocation ID
// the answer event should share.
func (o *Orchestrator) persistUserMessage(ctx context.Context, userID, sessionID, message string) string {
	invID := uuid.NewString()
	sess, err := o.getOrCreateSession(ctx, userID, sessionID)
	if err != nil {
		log.Printf("orchestrator: persist user message: get/create session: %v", err)
		return invID
	}
	userEv := session.NewEvent(invID)
	userEv.Author = "user"
	userEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
	if err := o.sessions.AppendEvent(ctx, sess, userEv); err != nil {
		log.Printf("orchestrator: persist user message: append event: %v", err)
	}
	return invID
}

// persistAnswer writes the final assistant answer to the "quack" ADK session,
// paired with the user event via the shared invocation ID.
func (o *Orchestrator) persistAnswer(ctx context.Context, userID, sessionID, invID string, nodes []dag.Node, finalAnswer string) {
	if strings.TrimSpace(finalAnswer) == "" {
		return
	}
	sess, err := o.getOrCreateSession(ctx, userID, sessionID)
	if err != nil {
		log.Printf("orchestrator: persist answer: get/create session: %v", err)
		return
	}

	// Author as the last node's agent name so store.Messages() maps it to "assistant".
	author := "synthesizer"
	for i := len(nodes) - 1; i >= 0; i-- {
		if strings.TrimSpace(nodes[i].AgentName) != "" {
			author = nodes[i].AgentName
			break
		}
	}

	answerEv := session.NewEvent(invID)
	answerEv.Author = author
	answerEv.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: finalAnswer}}}
	answerEv.TurnComplete = true
	if err := o.sessions.AppendEvent(ctx, sess, answerEv); err != nil {
		log.Printf("orchestrator: persist answer: append event: %v", err)
	}
}

func (o *Orchestrator) getOrCreateSession(ctx context.Context, userID, sessionID string) (session.Session, error) {
	resp, err := o.sessions.Get(ctx, &session.GetRequest{
		AppName: AppName, UserID: userID, SessionID: sessionID,
	})
	if err == nil && resp != nil {
		return resp.Session, nil
	}
	cr, err := o.sessions.Create(ctx, &session.CreateRequest{
		AppName: AppName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}
	return cr.Session, nil
}

// AgentClients is a convenience alias used by callers to pass the client map.
type AgentClients = map[string]adkagent.Agent
