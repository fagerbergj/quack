package agent

import (
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// MaxOutputTokens bounds generation so a reasoning model can't run away. Shared
// by every LLM agent Quack builds (the orchestrator dispatcher and each bundle
// agent) so their caps can't silently drift.
const MaxOutputTokens = 8192

// Build turns a loaded bundle into a runnable ADK llmagent, given its model,
// its selected built-in tools, and optional ADK toolsets (e.g. SkillToolset).
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset) (adkagent.Agent, error) {
	name, desc, behaviour := b.Card.Name, b.Card.Description, b.Prompt
	return llmagent.New(llmagent.Config{
		Name:        name,
		Description: desc,
		Model:       m,
		InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
			return promptbuilder.Agent(name, desc, tools, behaviour), nil
		},
		Tools:    tools,
		Toolsets: toolsets,
		// Summarize the older turns when a long agentic loop nears the context slot,
		// so the worker keeps its findings instead of overflowing and dying. Uses
		// the agent's own model for the (tool-less) summary call.
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{compactionCallback(m)},
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: MaxOutputTokens,
		},
	})
}
