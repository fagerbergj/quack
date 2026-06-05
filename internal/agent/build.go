package agent

import (
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// MaxOutputTokens bounds generation so a reasoning model can't run away. Shared
// by every LLM agent Quack builds (the orchestrator dispatcher and each bundle
// agent) so their caps can't silently drift.
const MaxOutputTokens = 8192

// Build turns a loaded bundle into a runnable ADK llmagent, given its model and
// its selected built-in tools. The resulting agent is what Serve exposes over
// A2A.
func Build(b *Bundle, m model.LLM, tools []tool.Tool) (adkagent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        b.Card.Name,
		Description: b.Card.Description,
		Model:       m,
		Instruction: b.Prompt,
		Tools:       tools,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: MaxOutputTokens,
		},
	})
}
