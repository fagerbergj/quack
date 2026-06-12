package dag

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"time" //nolint:godot

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Executor runs a Plan in topological order, concurrently within each layer.
// Each node is dispatched to its A2A client via a dedicated runner. Activity
// events stream live as they are produced; the node_id field routes each event
// to the correct node card in the frontend DAG view.
type Executor struct {
	sessions session.Service
	clients  map[string]adkagent.Agent // keyed by agent name
}

// NewExecutor returns an Executor. clients maps agent names to their A2A clients.
func NewExecutor(sessions session.Service, clients map[string]adkagent.Agent) *Executor {
	return &Executor{sessions: sessions, clients: clients}
}

// nodeMsg is one message sent from a node goroutine to the Execute main loop.
// When done=false it carries a live activity event; when done=true it signals
// that the goroutine has finished (output, err, and stats are set accordingly).
type nodeMsg struct {
	nodeID string
	ev     stream.SSEEvent
	output string
	err    error
	done   bool
	stats  stream.NodeDoneData // only meaningful when done=true
}

// Execute runs the plan and yields SSE events: DAG lifecycle events
// (node_queued/start/done/failed) plus activity events scoped to each node.
// Events are streamed live as they are produced — not buffered until completion.
// nodeOutputs accumulates the final text output of each node so the caller
// can extract the last node's text as the conversation's final answer.
func (e *Executor) Execute(ctx context.Context, plan Plan, userID string, nodeOutputs map[string]string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		layers, err := TopoSort(plan)
		if err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}

		// Nodes whose judge gate exhausted all rounds without passing. The DAG
		// continues (policy: continue-but-warn), but downstream nodes are told
		// their input failed vetting so they treat it skeptically.
		gateFailed := make(map[string]bool)

		for _, layer := range layers {
			// Announce all nodes in this layer as queued, then running, before
			// starting the goroutines so the frontend shows them simultaneously.
			for _, node := range layer {
				if !yield(stream.NodeQueued(node.ID), nil) {
					return
				}
			}
			for _, node := range layer {
				if !yield(stream.NodeStart(node.ID, node.AgentName), nil) {
					return
				}
			}

			// Derive a cancellable child context. Cancelling it stops all node
			// goroutines in the layer when the consumer disconnects or a node fails.
			layerCtx, cancelLayer := context.WithCancel(ctx)
			defer cancelLayer()

			// Snapshot upstream outputs and gate failures for the goroutines
			// (immutable read).
			upstream := make(map[string]string, len(nodeOutputs))
			for k, v := range nodeOutputs {
				upstream[k] = v
			}
			failedSnap := make(map[string]bool, len(gateFailed))
			for k, v := range gateFailed {
				failedSnap[k] = v
			}

			// Buffered enough to absorb a burst so goroutines rarely block.
			ch := make(chan nodeMsg, 256)
			for _, node := range layer {
				go func(n Node) {
					e.streamNode(layerCtx, plan, n, userID, upstream, failedSnap, ch)
				}(node)
			}

			// Relay events from the channel to the consumer.
			completed := 0
			for completed < len(layer) {
				select {
				case msg := <-ch:
					if msg.done {
						completed++
						if msg.err != nil {
							cancelLayer()
							yield(stream.NodeFailed(msg.nodeID, msg.err.Error()), nil)
							// Drain remaining completions so goroutines can exit.
							for completed < len(layer) {
								if m := <-ch; m.done {
									completed++
								}
							}
							return
						}
						nodeOutputs[msg.nodeID] = msg.output
						if msg.stats.JudgeRounds > 0 && !msg.stats.JudgePassed {
							gateFailed[msg.nodeID] = true
						}
						nd := msg.stats
						nd.OutputPreview = msg.output
						if len(nd.OutputPreview) > 250 {
							nd.OutputPreview = nd.OutputPreview[:250] + "…"
						}
						if !yield(stream.NodeDone(msg.nodeID, nd), nil) {
							cancelLayer()
							return
						}
					} else {
						if !yield(msg.ev, nil) {
							cancelLayer()
							return
						}
					}
				case <-ctx.Done():
					return
				}
			}
			cancelLayer()
		}
	}
}

// streamNode runs one node against its A2A client and sends all activity events
// to ch as they arrive, followed by a done message.
func (e *Executor) streamNode(ctx context.Context, plan Plan, node Node, userID string, upstream map[string]string, gateFailed map[string]bool, ch chan<- nodeMsg) {
	send := func(m nodeMsg) {
		select {
		case ch <- m:
		case <-ctx.Done():
		}
	}

	client, ok := e.clients[node.AgentName]
	if !ok {
		send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: unknown agent %q", node.ID, node.AgentName)})
		return
	}

	task := buildTask(plan, node, upstream, gateFailed)

	r, err := runner.New(runner.Config{
		AppName:           "quack-nodes",
		Agent:             client,
		SessionService:    e.sessions,
		AutoCreateSession: true,
	})
	if err != nil {
		send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: runner: %w", node.ID, err)})
		return
	}

	nodeSessionID := plan.ID + ":" + node.ID

	// Seed the node's fresh session with the prior conversation as native ADK
	// events — the runner assembles them into structured user/model turns in
	// the LLM request, which models follow better than a flattened transcript.
	if len(plan.History) > 0 {
		if err := e.seedHistory(ctx, userID, nodeSessionID, plan.History); err != nil {
			send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: seed history: %w", node.ID, err)})
			return
		}
	}

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: task}}}
	var answer strings.Builder
	var stats stream.NodeDoneData
	startedAt := time.Now()

	for ev, err := range r.Run(ctx, userID, nodeSessionID, content, adkagent.RunConfig{}) {
		if err != nil {
			send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: %w", node.ID, err)})
			return
		}
		for _, se := range stream.Translate(ev) {
			if se.Name == stream.EventAgentStart {
				continue
			}
			// agent_end carries completion metadata; accumulate before suppressing.
			if se.Name == stream.EventAgentEnd {
				if d, ok := se.Data.(stream.AgentData); ok {
					stats.PromptTokens += d.PromptTokens
					stats.CompletionTokens += d.CompletionTokens
					stats.ReasoningTokens += d.ReasoningTokens
					stats.TotalTokens += d.TotalTokens
					if d.Model != "" {
						stats.Model = d.Model
					}
					if d.FinishReason != "" {
						stats.FinishReason = d.FinishReason
					}
				}
				continue
			}
			// Accumulate gate metadata; these are forwarded to the frontend but
			// also summarised into NodeDoneData so the store can persist them.
			if se.Name == stream.EventSelfRefine {
				stats.SelfRefined = true
			}
			if se.Name == stream.EventJudgeVerdict {
				if d, ok := se.Data.(stream.JudgeVerdictData); ok {
					stats.JudgeRounds++
					stats.JudgeFinalScore = d.Score
					stats.JudgePassed = d.Passed
				}
			}
			scoped := stream.ScopeToNode(se, node.ID)
			send(nodeMsg{nodeID: node.ID, ev: scoped})
			if td, ok := scoped.Data.(stream.TokenData); ok {
				answer.WriteString(td.Text)
			}
		}
	}
	stats.DurationMs = time.Since(startedAt).Milliseconds()
	out := answer.String()
	if idx := strings.Index(out, "</think>"); idx >= 0 {
		out = strings.TrimLeft(out[idx+len("</think>"):], "\n")
	}
	send(nodeMsg{nodeID: node.ID, done: true, output: out, stats: stats})
}

// seedHistory creates the node's session pre-populated with the prior
// conversation as user/model events, so the runner presents them to the model
// as real turns. The session ID is unique per plan+node, so Create never races
// a prior run.
func (e *Executor) seedHistory(ctx context.Context, userID, sessionID string, history []HistoryTurn) error {
	cr, err := e.sessions.Create(ctx, &session.CreateRequest{
		AppName: "quack-nodes", UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	for _, t := range history {
		ev := session.NewEvent(uuid.NewString())
		if t.Role == "user" {
			ev.Author = "user"
		} else {
			ev.Author = "history"
		}
		ev.Content = &genai.Content{Role: t.Role, Parts: []*genai.Part{{Text: t.Text}}}
		if err := e.sessions.AppendEvent(ctx, cr.Session, ev); err != nil {
			return err
		}
	}
	return nil
}

// buildTask constructs the message for a node: the user's verbatim request
// first (the planner's task description is a lossy summary — details like
// names, dates, and constraints must reach the specialist directly), then
// upstream outputs, then the focused task. Conversation history is NOT inlined
// here — it is seeded into the node's session as native events (seedHistory).
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
