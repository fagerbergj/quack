package dag

import (
	"context"
	"fmt"
	"iter"
	"log"
	"strings"
	"time" //nolint:godot

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
	// sem caps how many nodes execute concurrently across all DAG runs. Nodes
	// whose dependencies are met still queue here until a slot frees, so a wide
	// layer doesn't fire N huge model requests at the single worker at once.
	sem chan struct{}
}

// NewExecutor returns an Executor. clients maps agent names to their A2A clients.
// maxActive caps concurrent node executions (<1 ⇒ default 2).
func NewExecutor(sessions session.Service, clients map[string]adkagent.Agent, maxActive int) *Executor {
	if maxActive < 1 {
		maxActive = 2
	}
	return &Executor{sessions: sessions, clients: clients, sem: make(chan struct{}, maxActive)}
}

// nodeMsg is one message sent from a node goroutine to the Execute main loop.
// start=true announces the node actually began (after acquiring a concurrency
// slot); done=false carries a live activity event; done=true signals the
// goroutine finished (output, err, and stats are set accordingly).
type nodeMsg struct {
	nodeID string
	ev     stream.SSEEvent
	output string
	err    error
	start  bool
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
			// Announce all nodes in this layer as queued. node_start is emitted later,
			// by each goroutine once it actually acquires a concurrency slot — so a
			// node capped behind the semaphore correctly shows "queued", not "running".
			for _, node := range layer {
				if !yield(stream.NodeQueued(node.ID), nil) {
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
					if msg.start {
						if !yield(msg.ev, nil) {
							cancelLayer()
							return
						}
						continue
					}
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

	// Acquire a concurrency slot: the node's deps are met, but it waits here behind
	// the max-active cap (it stays "queued" in the UI). Once a slot frees, emit
	// node_start and proceed. On cancellation, still report done so Execute's
	// completion accounting stays balanced.
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		send(nodeMsg{nodeID: node.ID, done: true, err: ctx.Err()})
		return
	}
	defer func() { <-e.sem }()
	send(nodeMsg{nodeID: node.ID, start: true, ev: stream.NodeStart(node.ID, node.AgentName)})

	client, ok := e.clients[node.AgentName]
	if !ok {
		send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: unknown agent %q", node.ID, node.AgentName)})
		return
	}

	task := buildTask(plan, node, upstream, gateFailed)
	// Diagnostic: how many of this node's upstream deps actually contributed a
	// non-empty output. 0/N on a node with deps means upstream produced nothing —
	// the input the synthesizer (or any dependent) is missing.
	if len(node.DependsOn) > 0 {
		filled := 0
		for _, dep := range node.DependsOn {
			if strings.TrimSpace(upstream[dep]) != "" {
				filled++
			}
		}
		log.Printf("dag: node %s built task from %d/%d upstream outputs (task_len=%d)", node.ID, filled, len(node.DependsOn), len(task))
	}

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

	// A node runs as a STATELESS worker in a fresh session (the runner
	// auto-creates it). Conversation context is NOT seeded here — the planner,
	// which has the history, writes self-contained tasks that inline whatever
	// prior content a node needs (see buildSystemPrompt). This keeps research
	// nodes lean and avoids dumping the whole transcript into every node.
	nodeSessionID := plan.ID + ":" + node.ID

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: task}}}
	var answer strings.Builder
	var stats stream.NodeDoneData
	startedAt := time.Now()
	translator := stream.NewTranslator()

	for ev, err := range r.Run(ctx, userID, nodeSessionID, content, adkagent.RunConfig{}) {
		if err != nil {
			send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: %w", node.ID, err)})
			return
		}
		for _, se := range translator.Event(ev) {
			// agent_complete carries each run's stats; summarise into NodeDoneData
			// (the store persists these; the worker run drives model/finish/usage).
			if se.Name == stream.EventAgentComplete {
				if d, ok := se.Data.(stream.AgentCompleteData); ok {
					stats.PromptTokens += d.PromptTokens
					stats.CompletionTokens += d.CompletionTokens
					stats.ReasoningTokens += d.ReasoningTokens
					stats.TotalTokens += d.TotalTokens
					if d.Model != "" {
						stats.Model = d.Model
					}
					switch d.Stage {
					case stream.StageWorker:
						if d.FinishReason != "" {
							stats.FinishReason = d.FinishReason
						}
					case stream.StageSelfRefine:
						stats.SelfRefined = true
					case stream.StageJudge:
						if d.Status == "" { // a completed verdict (not unavailable)
							stats.JudgeRounds++
							stats.JudgeFinalScore = d.Score
							stats.JudgePassed = d.Passed
						}
					}
				}
			}
			scoped := stream.ScopeToNode(se, node.ID)
			send(nodeMsg{nodeID: node.ID, ev: scoped})
			if td, ok := scoped.Data.(stream.AgentTokenData); ok {
				answer.WriteString(td.Text)
			}
		}
	}
	stats.DurationMs = time.Since(startedAt).Milliseconds()
	out := stream.StripThinking(answer.String())
	log.Printf("dag: node %s done output_len=%d judge_passed=%v judge_rounds=%d", node.ID, len(out), stats.JudgePassed, stats.JudgeRounds)
	send(nodeMsg{nodeID: node.ID, done: true, output: out, stats: stats})
}

// buildTask constructs the message for a node: the user's verbatim request
// first (the planner's task description is a lossy summary — details like
// names, dates, and constraints must reach the specialist directly), then
// upstream outputs, then the focused task. Nodes are stateless; any prior
// conversation a node needs is inlined into its task by the planner.
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
