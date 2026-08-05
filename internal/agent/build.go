package agent

import (
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// Build turns a loaded bundle into a runnable ADK llmagent, given its model,
// selected built-in tools, optional ADK toolsets, and optional context-
// compaction settings. When compaction is enabled and the model's context
// window is known, a BeforeModelCallback summarises + drops the session
// before each model call. memoryGuidance (bundle's memory.md, M6) is appended
// to the behaviour layer only for memory-participating agents; skills is the
// agent's declared skill scope (promptbuilder.Agent); grading is the
// pre-rendered trust-gate contract (promptbuilder.GradingFacts), "" when
// ungated or judge-less.
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string, skills []*skill.Frontmatter, grading string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, comp, memoryGuidance, skills, grading, "")
}

// BuildChat is Build with the agent's delegation mode PINNED to ModeChat at
// construction, for agents that run as a runner's ROOT over a multi-turn
// session (e.g. the advisor). Pinning matters beyond semantics: runner.Run
// force-sets an unset mode to ModeChat with an unsynchronized check-then-write
// on the shared agent - a data race when concurrent consults hit it at once.
// A pre-set mode turns that write into a pure read. Workers keep Build's
// unset mode, defaulting to single-turn task mode inside a workflow
// AgentNode, which is what the gate wants.
func BuildChat(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string, skills []*skill.Frontmatter, grading string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, comp, memoryGuidance, skills, grading, llmagent.ModeChat)
}

func build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, comp Compaction, memoryGuidance string, skills []*skill.Frontmatter, grading string, mode llmagent.Mode) (adkagent.Agent, error) {
	name, desc, behaviour := b.Card.Name, b.Card.Description, b.Prompt
	if g := strings.TrimSpace(memoryGuidance); g != "" {
		behaviour = behaviour + "\n\n" + g
	}
	cfg := llmagent.Config{
		Name:        name,
		Description: desc,
		Model:       m,
		InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
			// "" workspace: native bundles are never a coding agent (those run
			// as external ACP subprocesses - see internal/serve's ACP branch),
			// so there is no sandboxed clone/toolchain to state facts about.
			return promptbuilder.Agent(name, desc, tools, skills, behaviour, grading, ""), nil
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
