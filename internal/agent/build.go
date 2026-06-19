package agent

import (
	"strings"

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
// its selected built-in tools, optional ADK toolsets (e.g. SkillToolset), and
// optional context-compaction settings. When compaction is enabled and the
// model's context window is known, a BeforeModelCallback prunes/summarises the
// session before each model call so long runs can't overflow the window.
// memoryGuidance, when non-empty, is the bundle's memory.md appended to the
// behaviour layer (M6) — passed only for memory-participating agents when the
// feature is on, so it never dangles otherwise.
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string) (adkagent.Agent, error) {
	name, desc, behaviour := b.Card.Name, b.Card.Description, b.Prompt
	if g := strings.TrimSpace(memoryGuidance); g != "" {
		behaviour = behaviour + "\n\n" + g
	}
	cfg := llmagent.Config{
		Name:        name,
		Description: desc,
		Model:       m,
		InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
			return promptbuilder.Agent(name, desc, tools, behaviour), nil
		},
		Tools:    tools,
		Toolsets: toolsets,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: MaxOutputTokens,
		},
	}
	if comp.Enabled && comp.ContextWindow > 0 && comp.Summarizer != nil {
		cfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{compactionCallback(comp)}
		cfg.AfterModelCallbacks = []llmagent.AfterModelCallback{recordUsage()}
	}
	return llmagent.New(cfg)
}
