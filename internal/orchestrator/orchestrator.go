// Package orchestrator is Quack's request entrypoint. In M0 it is a stub: a
// single ADK llmagent run via the runner that answers directly. There is no
// agent dispatch and no DAG yet (those arrive in M1/M3). It owns the runner,
// and therefore the SessionService, so conversation turns persist.
package orchestrator

import (
	"context"
	"iter"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// AppName is the ADK application name used for all sessions.
const AppName = "quack"

// maxOutputTokens bounds generation so a reasoning model can't run away.
const maxOutputTokens = 8192

// Orchestrator runs a single LLM agent and streams its events.
type Orchestrator struct {
	runner *runner.Runner
}

// timeArgs is the (empty) input for the current_time tool.
type timeArgs struct{}

// currentTimeTool is a trivial built-in tool so M0 exercises tool calls.
func currentTimeTool() (tool.Tool, error) {
	return functiontool.New[timeArgs, string](
		functiontool.Config{
			Name:        "current_time",
			Description: "Return the current date and time (RFC3339, UTC).",
		},
		func(_ tool.Context, _ timeArgs) (string, error) {
			return time.Now().UTC().Format(time.RFC3339), nil
		},
	)
}

// New builds the orchestrator from a model, a session service, and a system
// instruction.
func New(m model.LLM, sessions session.Service, instruction string) (*Orchestrator, error) {
	timeTool, err := currentTimeTool()
	if err != nil {
		return nil, err
	}
	ag, err := llmagent.New(llmagent.Config{
		Name:        "orchestrator",
		Description: "Quack orchestrator (M0 stub: answers directly).",
		Model:       m,
		Instruction: instruction,
		Tools:       []tool.Tool{timeTool},
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: maxOutputTokens,
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
// session and yields the ADK session events as the agent produces them.
func (o *Orchestrator) Run(ctx context.Context, userID, sessionID, message string) iter.Seq2[*session.Event, error] {
	content := &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: message}},
	}
	return o.runner.Run(ctx, userID, sessionID, content, agent.RunConfig{})
}
