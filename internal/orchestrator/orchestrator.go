// Package orchestrator is Quack's request entrypoint. In M1 it is a thin LLM
// dispatcher: an ADK llmagent that delegates to config-defined specialist agents
// over A2A (each agent is a sub-agent backed by a remote A2A client). It does no
// research itself — it routes the request to the right agent and streams that
// agent's activity back. There is still no DAG (single dispatch); planning and
// decomposition arrive in M3.
//
// The orchestrator owns the runner, and therefore the SessionService, so
// conversation turns — including the delegated agent's events, which arrive over
// A2A already converted back to session events — persist.
package orchestrator

import (
	"context"
	"iter"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
)

// AppName is the ADK application name used for all sessions.
const AppName = "quack"

// Orchestrator runs the dispatcher agent and streams its (and its delegates')
// events.
type Orchestrator struct {
	runner *runner.Runner
}

// New builds the orchestrator from its own dispatcher model, a session service,
// a system instruction, the specialist sub-agents it can delegate to (A2A
// clients), and optional toolsets (e.g. a SkillToolset). With no sub-agents the
// dispatcher simply answers directly.
func New(m model.LLM, sessions session.Service, instruction string, subAgents []agent.Agent, toolsets []tool.Toolset) (*Orchestrator, error) {
	ag, err := llmagent.New(llmagent.Config{
		Name:        "orchestrator",
		Description: "Quack orchestrator: routes requests to specialist agents.",
		Model:       m,
		Instruction: instruction,
		SubAgents:   subAgents,
		Toolsets:    toolsets,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: quackagent.MaxOutputTokens,
		},
	})
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Config{
		AppName:           AppName,
		Agent:             ag,
		SessionService:    sessions,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, err
	}
	return &Orchestrator{runner: r}, nil
}

// Run executes one conversation turn: it sends the user message under the given
// session and yields the ADK session events as the orchestrator and any
// delegated agent produce them.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, message string) iter.Seq2[*session.Event, error] {
	content := &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: message}},
	}
	return o.runner.Run(ctx, userID, sessionID, content, agent.RunConfig{})
}
