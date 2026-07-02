// Package orchestrator is Quack's request entrypoint. It runs as a real ADK
// llmagent with two tools: plan (decomposes a query into a DAG) and execute
// (runs the DAG). Simple conversational queries are answered directly by the
// agent without calling either tool; research queries go through plan → execute.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// AppName is the ADK application name for the orchestrator's persistent chat
// session (the quack namespace, separate from each specialist node's session).
const AppName = "quack"

// orchestratorName is the ADK agent name + the author stamped on the
// orchestrator's persisted assistant messages (Run's deliver-persist), kept in
// one place so the two can't drift.
const orchestratorName = "orchestrator"

// Orchestrator is a real ADK llmagent that decides whether to answer directly
// from session context or to call plan → execute for web research.
type Orchestrator struct {
	sessions  session.Service
	model     model.LLM
	sysPrompt string
	planner   *dag.Planner
	executor  *dag.Executor
	skillTS   tool.Toolset  // optional; nil = no skill tools
	userMem   *memory.Store // optional user-memory store (M6); nil = user memory off
}

// CancelNode stops one running node of the chat's active run (continue-but-warn:
// the rest of the DAG keeps going). Returns false if no such live node. chatID is
// the session id used while executing (the executor registers node controls under
// it). Cooperative: takes effect at the node's next gate-stage boundary.
func (o *Orchestrator) CancelNode(chatID, nodeID string) bool {
	return o.executor.CancelNode(chatID, nodeID)
}

// SteerNode re-runs a single running node with new guidance. Returns false if no
// such live node. chatID is the session id used while executing. Cooperative:
// takes effect at the node's next gate-stage boundary.
func (o *Orchestrator) SteerNode(chatID, nodeID, guidance string) bool {
	return o.executor.SteerNode(chatID, nodeID, guidance)
}

// New builds the orchestrator. sysPrompt is assembled from agents/orchestrator/
// via promptbuilder.Orchestrator at startup. skillTS may be nil. userMem, when
// non-nil, enables personal memory: ambient recall (preload_memory) + an explicit
// commit_memory tool, both scoped to the user_memory collection.
func New(sessions session.Service, m model.LLM, sysPrompt string, planner *dag.Planner, executor *dag.Executor, skillTS tool.Toolset, userMem *memory.Store) *Orchestrator {
	return &Orchestrator{
		sessions:  sessions,
		model:     m,
		sysPrompt: sysPrompt,
		planner:   planner,
		executor:  executor,
		skillTS:   skillTS,
		userMem:   userMem,
	}
}

// Run processes message as the orchestrator agent and yields SSE events.
// The ADK runner manages session persistence (history, user/assistant turns).
// SSE events emitted by the plan and execute tools (dag_plan, node events,
// agent activity) are forwarded via yield context so they appear in the stream.
// attachments carries media parts (images, audio) from the current turn; their
// presence is described to the orchestrator in text and the raw parts are
// threaded through the plan tool to the planner, which routes them to a
// media-capable node (the orchestrator model itself stays text/vision-only).
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, message string, attachments []*genai.Part, interactive bool) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		// One plan cache per run, shared by this run's plan and execute tools, so
		// execute looks the plan up by ID instead of the model copying plan JSON.
		planCache := tools.NewPlanCache()
		// Persisted session, read BEFORE the runner appends this turn's user
		// message — so it holds only earlier turns. Used for both the planner's
		// history and pending-clarification detection below.
		prior := o.priorEvents(ctx, userID, sessionID)
		// Threaded to the planner so a re-plan after a clarifying exchange (or any
		// follow-up) resolves references against what was already said.
		history := buildHistory(prior)
		planTool, err := tools.NewPlanTool(o.planner, planCache, attachments, history, message)
		if err != nil {
			yield(stream.Errorf("orchestrator: plan tool: "+err.Error()), nil)
			return
		}
		execTool, err := tools.NewExecuteTool(planCache)
		if err != nil {
			yield(stream.Errorf("orchestrator: execute tool: "+err.Error()), nil)
			return
		}
		// Clarification tool: a long-running ask that pauses the turn until the
		// user picks an option (its answer is resumed below).
		choiceTool, err := tools.NewGetUserChoiceTool()
		if err != nil {
			yield(stream.Errorf("orchestrator: choice tool: "+err.Error()), nil)
			return
		}

		var toolsets []tool.Toolset
		if o.skillTS != nil {
			toolsets = []tool.Toolset{o.skillTS}
		}

		// User memory (M6, off by default): ambient recall via preload_memory plus
		// an explicit commit_memory tool, both scoped to this user's store. memSvc
		// stays a true nil interface when memory is off (no typed-nil hazard).
		toolList := []tool.Tool{planTool, execTool, choiceTool}
		var memSvc adkmemory.Service
		if o.userMem != nil {
			commitTool, err := tools.NewCommitMemoryTool(o.userMem, userID)
			if err != nil {
				yield(stream.Errorf("orchestrator: commit_memory tool: "+err.Error()), nil)
				return
			}
			toolList = append(toolList, memory.NewPreload(), commitTool)
			memSvc = o.userMem
		}

		ag, err := llmagent.New(llmagent.Config{
			Name:        orchestratorName,
			Description: "Routes research requests to specialist agents and answers conversational queries directly.",
			Model:       o.model,
			Instruction: o.sysPrompt,
			Tools:       toolList,
			Toolsets:    toolsets,
		})
		if err != nil {
			yield(stream.Errorf("orchestrator: build agent: "+err.Error()), nil)
			return
		}

		// One-orchestrator-workflow: the routing/planning/clarification llmagent and
		// the DAG run in ONE runner. The llmagent is the first node; a following
		// execute node runs the plan it selected (execute tool → ExecPlanIDKey) via
		// runDAG in this same runner — so an empty node pauses the whole run for human
		// steer/cancel natively, with no separate DAG runner to bridge.
		agentNode, err := workflow.NewAgentNode(ag, workflow.NodeConfig{})
		if err != nil {
			yield(stream.Errorf("orchestrator: agent node: "+err.Error()), nil)
			return
		}
		nodeOutputs := make(map[string]string)
		execNode := workflow.NewDynamicNode[any, string]("__execute",
			func(nctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
				v, _ := nctx.State().Get(tools.ExecPlanKey)
				planJSON, _ := v.(string)
				if planJSON == "" {
					return "", nil // the llmagent answered directly — no DAG to run
				}
				var plan dag.Plan
				if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
					return "", fmt.Errorf("execute node: unmarshal plan: %w", err)
				}
				outputs, rerr := o.executor.RunPlanInNode(nctx, plan, sessionID, interactive)
				if rerr != nil {
					return "", rerr // ErrNodeInterrupted → pause the run for steer/cancel
				}
				answer := tools.TerminalOutput(plan, outputs)
				planCache.SetResult(plan.ID, answer)
				planCache.SetDelivered(answer)
				return answer, nil
			}, workflow.NodeConfig{})
		wf, err := workflowagent.New(workflowagent.Config{
			Name:  "orchestrator-workflow",
			Edges: workflow.Chain(workflow.Start, agentNode, execNode),
		})
		if err != nil {
			yield(stream.Errorf("orchestrator: workflow: "+err.Error()), nil)
			return
		}

		r, err := runner.New(runner.Config{
			AppName:           AppName,
			Agent:             wf,
			SessionService:    o.sessions,
			MemoryService:     memSvc,
			AutoCreateSession: true,
		})
		if err != nil {
			yield(stream.Errorf("orchestrator: runner: "+err.Error()), nil)
			return
		}

		// Inject yield into context so the plan tool can forward its dag_plan SSE
		// event up through this stream without going through the ADK session pipeline.
		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { yield(ev, nil) })

		// Tell the orchestrator (in text) that media is attached so it routes to
		// research/plan rather than claiming it cannot see the file. The raw bytes
		// reach the media-capable node via the plan tool → planner → executor.
		text := message
		if desc := dag.AttachmentDesc(attachments); desc != "" {
			text += "\n\n" + desc
		}
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}

		// Resume a pending clarification: if the previous turn ended on an
		// unanswered get_user_choice call, deliver THIS message as that call's
		// answer (a FunctionResponse with the matching call ID) rather than a fresh
		// user turn. The model then continues from where it paused with the choice
		// resolved, instead of re-reading an open question. (The orchestrator runs
		// as a direct llmagent — not over A2A — so there is no adka2a layer to do
		// this; we hand-roll it here. See ChoiceToolName's TODO.) Any attachments on
		// a clarification-answer turn are dropped — answering a choice with a file is
		// not a supported flow.
		if callID := pendingChoiceCallID(prior); callID != "" {
			content = &genai.Content{Role: "user", Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					ID:       callID,
					Name:     tools.ChoiceToolName,
					Response: map[string]any{tools.ChoiceAnswerKey: message},
				},
			}}}
		}
		translator := stream.NewTranslator()

		// The orchestrator runs un-gated, so the translator never emits the
		// agent_start marker that gives node workers a run_id; its own thinking and
		// plan/execute tool calls would come out with an empty run_id and be dropped
		// by the client. Open one top-level run here and stamp the orchestrator's
		// events onto it (ScopeToRun) so its activity renders as the agent wrapping
		// the DAG. node events forwarded by the execute tool already carry their own
		// run_id/node_id and are untouched.
		const orchRunID = "orchestrator"
		yield(stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{
			RunID: orchRunID, Agent: "orchestrator", Stage: stream.StageWorker,
		}}, nil)

		// Gate-node lifecycle SSE can be emitted from concurrent workflow goroutines;
		// serialize all yields through one mutex.
		var mu sync.Mutex
		safeYield := func(ev stream.SSEEvent, e error) bool { mu.Lock(); defer mu.Unlock(); return yield(ev, e) }
		var ds *dag.DagStream
		for ev, err := range r.Run(ctx, userID, sessionID, content, adkagent.RunConfig{}) {
			if err != nil {
				safeYield(stream.Errorf(err.Error()), nil)
				return
			}
			if ev == nil {
				continue
			}
			// Once the plan tool has cached a plan, route its gate-node events to the
			// DAG stream; everything else is the orchestrator's own thinking/tool activity.
			if ds == nil {
				if p, ok := planCache.Latest(); ok {
					ds = o.executor.NewDagStream(ctx, p, AppName, userID, sessionID, safeYield, nodeOutputs)
				}
			}
			if ds != nil && ds.Handle(ev) {
				continue
			}
			for _, se := range translator.Event(ev) {
				if !safeYield(stream.ScopeToRun(se, orchRunID), nil) {
					return
				}
			}
		}
		if ds != nil {
			ds.Finish()
		}
		yield(stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{
			RunID: orchRunID, Stage: stream.StageWorker,
		}}, nil)

		// Deliver: the execute node streamed the plan's answer straight to the user
		// and the orchestrator stayed silent, so its session holds no record of the
		// answer. Append it as the assistant message so it
		// survives reload (AsstText) and is visible to follow-up turns. Use a
		// detached context so a client disconnect at end-of-stream still persists.
		if delivered := planCache.Delivered(); delivered != "" {
			persistCtx := context.WithoutCancel(ctx)
			if resp, gerr := o.sessions.Get(persistCtx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID}); gerr == nil && resp != nil {
				aev := session.NewEvent(persistCtx, "")
				aev.Author = ag.Name()
				aev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: delivered}}}
				_ = o.sessions.AppendEvent(persistCtx, resp.Session, aev)
			}
		}
		yield(stream.Done(), nil)
	}
}

// priorEvents reads the persisted session's events (nil if the session is
// missing). Called BEFORE the runner appends the current turn, so it holds only
// earlier turns; shared by buildHistory and pendingChoiceCallID so one Run reads
// the session once.
func (o *Orchestrator) priorEvents(ctx context.Context, userID, sessionID string) []*session.Event {
	resp, err := o.sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil || resp == nil {
		return nil
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	return events
}

// buildHistory converts prior session events into dag.HistoryTurn values for the
// planner. The current message reaches the planner separately as the plan query.
//
// It tolerates a half-finished turn: a user turn with no assistant reply yet —
// e.g. an upfront clarifying question still awaiting an answer, or a DAG paused
// mid-run (M5 pause/resume) — contributes its user text and simply no model
// text, so the planner still sees the open question. Thinking and tool
// call/response parts are dropped (the planner is a raw text LLM call).
func buildHistory(events []*session.Event) []dag.HistoryTurn {
	var turns []dag.HistoryTurn
	var userText, modelText strings.Builder
	haveTurn := false
	// flush emits the accumulated turn (user line, then model line) skipping
	// empty halves, so a half-finished turn yields just its user line.
	flush := func() {
		if !haveTurn {
			return
		}
		if t := strings.TrimSpace(userText.String()); t != "" {
			turns = append(turns, dag.HistoryTurn{Role: "user", Text: t})
		}
		if t := strings.TrimSpace(modelText.String()); t != "" {
			turns = append(turns, dag.HistoryTurn{Role: "model", Text: t})
		}
	}

	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Author == "user" {
			flush()
			userText.Reset()
			modelText.Reset()
			haveTurn = true
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
					userText.WriteString(p.Text)
				}
			}
		} else if haveTurn {
			for _, p := range ev.Content.Parts {
				if p == nil || p.Thought || p.FunctionCall != nil || p.FunctionResponse != nil {
					continue
				}
				modelText.WriteString(p.Text)
			}
		}
	}
	flush()
	return turns
}

// pendingChoiceCallID returns the call ID of an unanswered get_user_choice
// clarification in the prior events, or "" if none is awaiting an answer. It
// scans for the most recent choice FunctionCall and clears it once a real answer
// (a FunctionResponse carrying tools.ChoiceAnswerKey) follows. The framework
// auto-emits a "pending" placeholder FunctionResponse for every long-running
// call, so presence of a response alone does not mean answered — only one
// carrying the answer key does.
func pendingChoiceCallID(events []*session.Event) string {
	var pendingID string
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == tools.ChoiceToolName {
				pendingID = p.FunctionCall.ID
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == tools.ChoiceToolName && p.FunctionResponse.ID == pendingID {
				if _, answered := p.FunctionResponse.Response[tools.ChoiceAnswerKey]; answered {
					pendingID = ""
				}
			}
		}
	}
	return pendingID
}

// AgentClients is a convenience alias used by callers to pass the client map.
type AgentClients = map[string]adkagent.Agent
