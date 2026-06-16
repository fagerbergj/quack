// Package orchestrator is Quack's request entrypoint. It runs as a real ADK
// llmagent with two tools: plan (decomposes a query into a DAG) and execute
// (runs the DAG). Simple conversational queries are answered directly by the
// agent without calling either tool; research queries go through plan → execute.
package orchestrator

import (
	"context"
	"iter"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	internalagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// AppName is the ADK application name for the orchestrator's persistent chat
// session (the quack namespace, separate from each specialist node's session).
const AppName = "quack"

// Orchestrator is a real ADK llmagent that decides whether to answer directly
// from session context or to call plan → execute for web research.
type Orchestrator struct {
	sessions  session.Service
	model     model.LLM
	sysPrompt string
	planner   *dag.Planner
	executor  *dag.Executor
	skillTS   tool.Toolset // optional; nil = no skill tools
}

// New builds the orchestrator. sysPrompt is assembled from agents/orchestrator/
// via promptbuilder.Orchestrator at startup. skillTS may be nil.
func New(sessions session.Service, m model.LLM, sysPrompt string, planner *dag.Planner, executor *dag.Executor, skillTS tool.Toolset) *Orchestrator {
	return &Orchestrator{
		sessions:  sessions,
		model:     m,
		sysPrompt: sysPrompt,
		planner:   planner,
		executor:  executor,
		skillTS:   skillTS,
	}
}

// Run processes message as the orchestrator agent and yields SSE events.
// The ADK runner manages session persistence (history, user/assistant turns).
// SSE events emitted by the plan and execute tools (dag_plan, node events,
// agent activity) are forwarded via yield context so they appear in the stream.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, message string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		planTool, err := tools.NewPlanTool(o.planner)
		if err != nil {
			yield(stream.Errorf("orchestrator: plan tool: "+err.Error()), nil)
			return
		}
		execTool, err := tools.NewExecuteTool(o.executor, userID)
		if err != nil {
			yield(stream.Errorf("orchestrator: execute tool: "+err.Error()), nil)
			return
		}

		var toolsets []tool.Toolset
		if o.skillTS != nil {
			toolsets = []tool.Toolset{o.skillTS}
		}

		ag, err := llmagent.New(llmagent.Config{
			Name:        "orchestrator",
			Description: "Routes research requests to specialist agents and answers conversational queries directly.",
			Model:       o.model,
			Instruction: o.sysPrompt,
			Tools:       []tool.Tool{planTool, execTool},
			Toolsets:    toolsets,
			GenerateContentConfig: &genai.GenerateContentConfig{
				MaxOutputTokens: internalagent.MaxOutputTokens,
			},
		})
		if err != nil {
			yield(stream.Errorf("orchestrator: build agent: "+err.Error()), nil)
			return
		}

		r, err := runner.New(runner.Config{
			AppName:           AppName,
			Agent:             ag,
			SessionService:    o.sessions,
			AutoCreateSession: true,
		})
		if err != nil {
			yield(stream.Errorf("orchestrator: runner: "+err.Error()), nil)
			return
		}

		// Inject yield into context so the plan and execute tools can forward
		// SSE events (dag_plan, node_queued/start/done, agent activity) up
		// through this stream without going through the ADK session pipeline.
		ctx = stream.WithYield(ctx, func(ev stream.SSEEvent) { yield(ev, nil) })

		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
		translator := stream.NewTranslator()

		for ev, err := range r.Run(ctx, userID, sessionID, content, adkagent.RunConfig{}) {
			if err != nil {
				yield(stream.Errorf(err.Error()), nil)
				return
			}
			for _, se := range translator.Event(ev) {
				if !yield(se, nil) {
					return
				}
			}
		}
		yield(stream.Done(), nil)
	}
}

// lastOutput returns the output of the terminal node (no successors) in the
// plan. Falls back to the last node in slice order. Used by tests.
func lastOutput(plan *dag.Plan, outputs map[string]string) string {
	hasSuccessor := make(map[string]bool, len(plan.Nodes))
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
	for i := len(plan.Nodes) - 1; i >= 0; i-- {
		if out, ok := outputs[plan.Nodes[i].ID]; ok {
			return out
		}
	}
	return ""
}

// AgentClients is a convenience alias used by callers to pass the client map.
type AgentClients = map[string]adkagent.Agent
