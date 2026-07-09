package agent

import (
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// Build turns a loaded bundle into a runnable ADK llmagent, given its model,
// its selected built-in tools, optional ADK toolsets (e.g. SkillToolset), and
// optional context-compaction settings. When compaction is enabled and the
// model's context window is known, a BeforeModelCallback prunes/summarises the
// session before each model call so long runs can't overflow the window.
// memoryGuidance, when non-empty, is the bundle's memory.md appended to the
// behaviour layer (M6) — passed only for memory-participating agents when the
// feature is on, so it never dangles otherwise.
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, comp, memoryGuidance, "")
}

// BuildChat is Build with the agent's delegation mode PINNED to ModeChat at
// construction. Use it for agents that run as a runner's ROOT over a
// multi-turn session (the advisor: internal/tools/ask_advisor.go spins a
// runner per consult over ONE shared agent instance). Pinning matters beyond
// semantics: runner.Run force-sets an unset mode to ModeChat with an
// unsynchronized check-then-write on the shared agent — a data race when two
// consults from concurrently-running nodes hit it at once. A pre-set mode
// turns that write into a pure read. Workers keep Build's unset mode: wrapped
// in a workflow AgentNode they default to single-turn task mode (prompt-only,
// no session history), which is exactly what the gate wants.
func BuildChat(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, comp, memoryGuidance, llmagent.ModeChat)
}

func build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string, mode llmagent.Mode) (adkagent.Agent, error) {
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
		Mode:     mode,
	}
	if comp.Enabled && comp.ContextWindow > 0 && comp.Summarizer != nil {
		cfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{compactionCallback(comp)}
		cfg.AfterModelCallbacks = []llmagent.AfterModelCallback{recordUsage()}
	}
	return llmagent.New(cfg)
}
