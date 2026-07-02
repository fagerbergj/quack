package dag

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"time" //nolint:godot

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Executor runs a Plan by readiness: each node fires as soon as all its
// dependencies are done, so independent branches make progress concurrently
// rather than waiting on a per-layer barrier (bounded by the max-active
// semaphore). Each node is dispatched to its A2A client via a dedicated runner.
// Activity events stream live as they are produced; the node_id field routes
// each event to the correct node card in the frontend DAG view.
type Executor struct {
	sessions    session.Service
	clients     map[string]adkagent.Agent // keyed by agent name
	mediaAgents map[string]bool           // agents that accept image/audio InlineData parts
	// sem caps how many nodes execute concurrently across all DAG runs. Nodes
	// whose dependencies are met still queue here until a slot frees, so a wide
	// layer doesn't fire N huge model requests at the single worker at once.
	sem chan struct{}

	// mu guards nodeCancels and steers: chatID → nodeID → … for the in-flight run.
	// CancelNode (called from the REST handler) cancels one node's context so the
	// run continues without it (continue-but-warn), distinct from cancelling the
	// whole run. The inner maps are created when a run starts and dropped when it
	// ends, so controlling a finished node is a harmless no-op.
	mu          sync.Mutex
	nodeCancels map[string]map[string]context.CancelFunc
	// steers holds queued steer guidance: chatID → nodeID → guidance. SteerNode
	// stores the guidance then cancels the node's context; the Execute loop, on
	// seeing that cancel, re-runs the node with the guidance (against its same
	// session, so prior tool calls/results are kept) instead of failing it.
	steers map[string]map[string]string
}

// nodeAppName is the runner AppName for DAG nodes; the steer path reuses it to
// reach a node's session for sanitising before a re-run.
const nodeAppName = "quack-nodes"

// NewExecutor returns an Executor. clients maps agent names to their A2A clients.
// mediaAgents is the set of agent names that accept image/audio parts in their content.
// maxActive caps concurrent node executions (<1 ⇒ default 2).
func NewExecutor(sessions session.Service, clients map[string]adkagent.Agent, mediaAgents map[string]bool, maxActive int) *Executor {
	if maxActive < 1 {
		maxActive = 2
	}
	return &Executor{
		sessions: sessions, clients: clients, mediaAgents: mediaAgents,
		sem:         make(chan struct{}, maxActive),
		nodeCancels: make(map[string]map[string]context.CancelFunc),
		steers:      make(map[string]map[string]string),
	}
}

// CancelNode stops a single running node of chatID's active run. The node ends as
// if its work failed (continue-but-warn: downstream still runs, told its input is
// missing), NOT cancelling the whole run. Returns false if no such live node.
func (e *Executor) CancelNode(chatID, nodeID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.nodeCancels[chatID]; ok {
		if cancel, ok := m[nodeID]; ok {
			cancel()
			return true
		}
	}
	return false
}

// SteerNode interrupts a single running node of chatID's active run and queues
// guidance so the Execute loop re-runs that node against its same session (its
// prior tool calls and results are retained; the worker revises on top of them).
// Returns false if no such live node. The node is NOT failed and its dependents
// keep waiting for the re-run.
func (e *Executor) SteerNode(chatID, nodeID, guidance string) bool {
	e.mu.Lock()
	m, ok := e.nodeCancels[chatID]
	if !ok {
		e.mu.Unlock()
		return false
	}
	cancel, ok := m[nodeID]
	if !ok {
		e.mu.Unlock()
		return false
	}
	if s := e.steers[chatID]; s != nil {
		s[nodeID] = guidance
	}
	e.mu.Unlock()
	cancel() // interrupt the in-flight invocation; Execute sees the cancel + the queued steer
	return true
}

// takeSteer returns and removes any queued steer guidance for a node.
func (e *Executor) takeSteer(chatID, nodeID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.steers[chatID]; ok {
		if g, ok := s[nodeID]; ok {
			delete(s, nodeID)
			return g, true
		}
	}
	return "", false
}

func (e *Executor) registerRun(chatID string) {
	e.mu.Lock()
	e.nodeCancels[chatID] = make(map[string]context.CancelFunc)
	e.steers[chatID] = make(map[string]string)
	e.mu.Unlock()
}

func (e *Executor) unregisterRun(chatID string) {
	e.mu.Lock()
	delete(e.nodeCancels, chatID)
	delete(e.steers, chatID)
	e.mu.Unlock()
}

func (e *Executor) registerNode(chatID, nodeID string, cancel context.CancelFunc) {
	e.mu.Lock()
	if m, ok := e.nodeCancels[chatID]; ok {
		m[nodeID] = cancel
	}
	e.mu.Unlock()
}

// nodeMsg is one message sent from a node goroutine to the Execute main loop.
// start=true announces the node actually began (after acquiring a concurrency
// slot); done=false carries a live activity event; done=true signals the
// goroutine finished (output, err, and stats are set accordingly).
type nodeMsg struct {
	nodeID    string
	ev        stream.SSEEvent
	output    string
	err       error
	start     bool
	done      bool
	cancelled bool                // node was individually cancelled (not a failure, not whole-run cancel)
	stats     stream.NodeDoneData // only meaningful when done=true
}

// Execute runs the plan from scratch and yields SSE events: DAG lifecycle events
// (node_queued/start/done/failed) plus activity events scoped to each
// node. Events stream live as produced — not buffered until completion.
// nodeOutputs accumulates the final text output of each node so the caller can
// extract the terminal node's text as the conversation's final answer.
//
// Scheduling is readiness-driven: a node is launched the instant all its
// dependencies are done, not at a layer boundary. Every launched goroutine
// self-limits on the max-active semaphore (staying "queued" in the UI until a
// slot frees), so launching the whole ready frontier at once is safe.
func (e *Executor) Execute(ctx context.Context, plan Plan, userID, chatID string, nodeOutputs map[string]string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		// TopoSort is used only to validate the plan (cycles, unknown deps); the
		// scheduler below runs by readiness, not by the returned layers.
		if _, err := TopoSort(plan); err != nil {
			yield(stream.Errorf("dag: "+err.Error()), nil)
			return
		}

		// Register this run so the REST handler can cancel individual nodes by
		// (chatID, nodeID); drop the whole map when the run ends.
		e.registerRun(chatID)
		defer e.unregisterRun(chatID)

		// remainingDeps[id] = how many of node id's dependencies are not yet done;
		// a node is runnable at 0. Counts occurrences, matching TopoSort's in-degree.
		remainingDeps := make(map[string]int, len(plan.Nodes))
		for _, n := range plan.Nodes {
			remainingDeps[n.ID] = len(n.DependsOn)
		}

		// Nodes whose judge gate exhausted all rounds without passing. The DAG
		// continues (policy: continue-but-warn), but downstream nodes are told
		// their input failed vetting so they treat it skeptically. A gate failure
		// does NOT block downstream — the node is still "done" with a warning flag.
		gateFailed := make(map[string]bool)
		launched := make(map[string]bool)

		// One cancellable context for every node goroutine: cancelling stops the
		// whole run when the consumer disconnects or a node fails.
		ctx, cancelAll := context.WithCancel(ctx)
		defer cancelAll()

		// Buffered enough to absorb a burst so goroutines rarely block.
		ch := make(chan nodeMsg, 256)

		// nodesByID indexes the plan so a steer can re-launch a single node by ID.
		nodesByID := make(map[string]Node, len(plan.Nodes))
		for _, n := range plan.Nodes {
			nodesByID[n.ID] = n
		}

		// decrementDependents lowers the remaining-dep count of every node that
		// depends on id — called when id becomes (or is pre-seeded as) done.
		decrementDependents := func(id string) {
			for _, n := range plan.Nodes {
				for _, dep := range n.DependsOn {
					if dep == id {
						remainingDeps[n.ID]--
					}
				}
			}
		}

		completed := 0

		// launchNode starts one node's goroutine (node_start is emitted later, by
		// the goroutine once it acquires a concurrency slot). steer is "" for a
		// normal launch, or the guidance for a steer re-run (same session reused).
		// Returns false if the consumer disconnected.
		launchNode := func(node Node, steer string) bool {
			if !yield(stream.NodeQueued(node.ID), nil) {
				return false
			}
			launched[node.ID] = true
			// Immutable per-goroutine snapshot of upstream outputs + gate failures.
			// All of this node's deps are done, so their outputs are present; the
			// maps are mutated only here in the main loop.
			upstream := make(map[string]string, len(nodeOutputs))
			for k, v := range nodeOutputs {
				upstream[k] = v
			}
			failedSnap := make(map[string]bool, len(gateFailed))
			for k, v := range gateFailed {
				failedSnap[k] = v
			}
			// Per-node context (child of the run ctx) so one node can be cancelled
			// without tearing down the run. Registered for CancelNode / SteerNode.
			nodeCtx, nodeCancel := context.WithCancel(ctx)
			e.registerNode(chatID, node.ID, nodeCancel)
			go func(n Node, nctx context.Context, st string) {
				e.streamNode(nctx, ctx, plan, n, userID, upstream, failedSnap, st, ch)
			}(node, nodeCtx, steer)
			return true
		}

		// launchReady starts every not-yet-launched node whose deps are all done.
		launchReady := func() bool {
			for _, node := range plan.Nodes {
				if launched[node.ID] || remainingDeps[node.ID] != 0 {
					continue
				}
				if !launchNode(node, "") {
					return false
				}
			}
			return true
		}

		// drain waits for every launched-but-unfinished goroutine to send its
		// terminal (done) message, so cancelled goroutines exit cleanly before we
		// return. len(launched) is stable once we stop calling launchReady.
		drain := func(completed int) {
			for completed < len(launched) {
				if m := <-ch; m.done {
					completed++
				}
			}
		}

		if !launchReady() {
			cancelAll()
			drain(completed)
			return
		}

		for completed < len(plan.Nodes) {
			select {
			case msg := <-ch:
				if msg.start {
					if !yield(msg.ev, nil) {
						cancelAll()
						drain(completed)
						return
					}
					continue
				}
				if !msg.done {
					if !yield(msg.ev, nil) {
						cancelAll()
						drain(completed)
						return
					}
					continue
				}
				// Terminal message for this node.
				completed++
				guidance, steered := e.takeSteer(chatID, msg.nodeID)
				if msg.cancelled {
					if steered {
						// Steered, not stopped: re-run this same node with the
						// guidance. Its session (prior tool calls + results) is reused
						// — nodeSessionID is derived from plan.ID+node.ID, unchanged.
						// Don't mark it done/failed and don't touch dependents (they
						// keep waiting). Only undo the completion count once the re-run
						// is actually launched, so a teardown before then still has the
						// old goroutine's terminal accounted for (drain stays balanced).
						if !yield(stream.NodeSteered(msg.nodeID, guidance), nil) {
							cancelAll()
							drain(completed)
							return
						}
						if !launchNode(nodesByID[msg.nodeID], guidance) {
							cancelAll()
							drain(completed)
							return
						}
						completed-- // re-run is live; this terminal wasn't a final completion
						continue
					}
					// One node cancelled (not the whole run): continue-but-warn. It
					// contributes no output; dependents are told their input is missing
					// (gateFailed) so they don't rely on it. The DAG keeps running.
					nodeOutputs[msg.nodeID] = ""
					gateFailed[msg.nodeID] = true
					if !yield(stream.NodeFailed(msg.nodeID, "cancelled by user"), nil) {
						cancelAll()
						drain(completed)
						return
					}
					decrementDependents(msg.nodeID)
					if !launchReady() {
						cancelAll()
						drain(completed)
						return
					}
					continue
				}
				if msg.err != nil {
					cancelAll()
					yield(stream.NodeFailed(msg.nodeID, msg.err.Error()), nil)
					drain(completed)
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
				nd.Output = msg.output
				if !yield(stream.NodeDone(msg.nodeID, nd), nil) {
					cancelAll()
					drain(completed)
					return
				}
				// This node finished: launch any dependents that just became runnable.
				decrementDependents(msg.nodeID)
				if !launchReady() {
					cancelAll()
					drain(completed)
					return
				}
			case <-ctx.Done():
				drain(completed)
				return
			}
		}
	}
}

// streamNode runs one node against its A2A client and sends all activity events
// to ch as they arrive, followed by a done message.
// streamNode runs one node. ctx is the node's own context (cancelled by
// CancelNode); parentCtx is the run's context (cancelled on whole-run stop). The
// two are distinguished so an individual node cancel ends as cancelled (the run
// continues) while a run-wide cancel ends as a plain run teardown.
func (e *Executor) streamNode(ctx, parentCtx context.Context, plan Plan, node Node, userID string, upstream map[string]string, gateFailed map[string]bool, steer string, ch chan<- nodeMsg) {
	// send blocks rather than racing ctx.Done: Execute always drains every
	// launched goroutine before returning, so the receiver is guaranteed and a
	// terminal (done/waiting) message can't be lost to a cancel race — which would
	// hang drain's completion count.
	send := func(m nodeMsg) { ch <- m }

	// cancelled reports an individual node cancel: this node's ctx is done but the
	// run's ctx is not (a whole-run stop cancels both).
	cancelled := func() bool { return ctx.Err() != nil && parentCtx.Err() == nil }
	endCtx := func() { // terminal msg for a ctx-cancelled node
		if cancelled() {
			send(nodeMsg{nodeID: node.ID, done: true, cancelled: true})
		} else {
			send(nodeMsg{nodeID: node.ID, done: true, err: ctx.Err()})
		}
	}

	// Acquire a concurrency slot: the node's deps are met, but it waits here behind
	// the max-active cap (it stays "queued" in the UI). Once a slot frees, emit
	// node_start and proceed. On cancellation, still report done so Execute's
	// completion accounting stays balanced.
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		endCtx()
		return
	}
	defer func() { <-e.sem }()
	send(nodeMsg{nodeID: node.ID, start: true, ev: stream.NodeStart(node.ID, node.AgentName)})

	client, ok := e.clients[node.AgentName]
	if !ok {
		send(nodeMsg{nodeID: node.ID, done: true, err: fmt.Errorf("node %q: unknown agent %q", node.ID, node.AgentName)})
		return
	}

	r, err := runner.New(runner.Config{
		AppName:           nodeAppName,
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

	var content *genai.Content
	if steer != "" {
		// Steer re-run: reuse the existing session — the prior task, draft, tool
		// calls and results are all there — and append the user's guidance as the
		// next turn. An interrupt can cut a turn off mid tool-call, leaving an
		// unanswered call the model server would reject, so close any such dangling
		// call first.
		e.sanitizeDanglingCalls(ctx, userID, nodeSessionID)
		content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: steerPrompt(steer)}}}
		slog.Info("node steered: re-running with guidance", "component", "dag", "node", node.ID, "guidance_len", len(steer))
	} else {
		// Build the node's task content.
		task := buildTask(plan, node, upstream, gateFailed)
		// Diagnostic: how many of this node's upstream deps actually contributed a
		// non-empty output. 0/N on a node with deps means upstream produced nothing.
		if len(node.DependsOn) > 0 {
			filled := 0
			for _, dep := range node.DependsOn {
				if strings.TrimSpace(upstream[dep]) != "" {
					filled++
				}
			}
			slog.Debug("node built task from upstream outputs", "component", "dag", "node", node.ID, "filled", filled, "deps", len(node.DependsOn), "task_len", len(task))
		}
		// Media-capable agents (image/audio inputs) receive the attachment parts
		// prepended before the text task so the model sees the file before the
		// instruction — the standard layout for multimodal VLMs.
		parts := []*genai.Part{{Text: task}}
		if e.mediaAgents[node.AgentName] && len(plan.Attachments) > 0 {
			parts = append(plan.Attachments, parts...)
			slog.Debug("node sending attachments to media agent", "component", "dag", "node", node.ID, "attachments", len(plan.Attachments), "agent", node.AgentName)
		}
		content = &genai.Content{Role: "user", Parts: parts}
	}
	var answer strings.Builder
	var stats stream.NodeDoneData
	startedAt := time.Now()
	translator := stream.NewTranslator()

	for ev, err := range r.Run(ctx, userID, nodeSessionID, content, adkagent.RunConfig{}) {
		if err != nil {
			if ctx.Err() != nil { // cancelled mid-run: classify individual vs run-wide
				endCtx()
				return
			}
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
	slog.Info("node done", "component", "dag", "node", node.ID, "output_len", len(out), "judge_passed", stats.JudgePassed, "judge_rounds", stats.JudgeRounds)
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

// steerPrompt wraps the user's mid-run guidance for a node re-run. The node's
// session already holds its prior work, so the instruction is to revise, not
// restart.
func steerPrompt(guidance string) string {
	return "The user is steering you mid-task with new guidance. Revise your work to follow it, " +
		"reusing the research and tool results you already have rather than starting over:\n\n" + guidance
}

// sanitizeDanglingCalls closes any function call left without a matching response
// in a node's session. An interrupt can cancel a turn mid tool-call, leaving an
// unanswered call; a request that ends on one is rejected by the model server
// ("function call without response"). ponytail: append a synthetic "interrupted"
// response so the steer re-run sees a complete, valid history (all real tool
// calls/results intact). Best-effort — a failure here just lets the re-run try.
func (e *Executor) sanitizeDanglingCalls(ctx context.Context, userID, sessionID string) {
	resp, err := e.sessions.Get(ctx, &session.GetRequest{AppName: nodeAppName, UserID: userID, SessionID: sessionID})
	if err != nil || resp == nil || resp.Session == nil {
		return
	}
	answered := make(map[string]bool)
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionResponse != nil {
				answered[p.FunctionResponse.ID] = true
			}
		}
	}
	var parts []*genai.Part
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.ID != "" && !answered[p.FunctionCall.ID] {
				parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
					ID:       p.FunctionCall.ID,
					Name:     p.FunctionCall.Name,
					Response: map[string]any{"status": "interrupted", "note": "cancelled by user steer before completion"},
				}})
				answered[p.FunctionCall.ID] = true // a call could appear twice; close it once
			}
		}
	}
	if len(parts) == 0 {
		return
	}
	aev := session.NewEvent(ctx, "")
	aev.Author = "user"
	aev.Content = &genai.Content{Role: "user", Parts: parts}
	if err := e.sessions.AppendEvent(ctx, resp.Session, aev); err != nil {
		slog.Warn("steer: could not close dangling tool calls", "component", "dag", "session", sessionID, "err", err)
	}
}
