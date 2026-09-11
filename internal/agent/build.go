package agent

import (
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// Build turns a loaded bundle into a runnable ADK llmagent, given its model, selected built-in tools, and optional ADK toolsets (context compaction is
// wired separately at the runner, see a2a.go's Serve). memoryGuidance (the bundle's memory.md, M6) is appended to the behaviour layer only for
// memory-participating agents; skills is the agent's declared skill scope (promptbuilder.Agent); grading is the pre-rendered trust-gate contract (promptbuilder.GradingFacts), "" when ungated or judge-less.
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, skills []*skill.Frontmatter, grading string, drain func() string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, memoryGuidance, skills, grading, "", drain)
}

// BuildChat is Build with the delegation mode PINNED to ModeChat, for agents
// running as a runner's ROOT over a multi-turn session (e.g. the advisor). Pinning matters: runner.Run force-sets an unset mode to ModeChat with an
// unsynchronized check-then-write on the shared agent - a data race under concurrent consults; a pre-set mode turns that write into a pure read. Workers keep Build's unset mode (single-turn task mode, what the gate wants).
func BuildChat(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, skills []*skill.Frontmatter, grading string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, memoryGuidance, skills, grading, llmagent.ModeChat, nil)
}

func build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, skills []*skill.Frontmatter, grading string, mode llmagent.Mode, drain func() string) (adkagent.Agent, error) {
	name, desc, behaviour := b.Card.Name, b.Card.Description, b.Prompt
	if g := strings.TrimSpace(memoryGuidance); g != "" {
		behaviour = behaviour + "\n\n" + g
	}
	// Every Agent() input below is fixed once build() returns except today() -
	// cache the assembled prompt instead of rebuilding it on every model call.
	prompt := promptbuilder.CacheByDay(func() string {
		// "" workspace: native bundles are never a coding agent (those run as external ACP subprocesses - see internal/serve's ACP branch),
		// so there is no sandboxed clone/toolchain to state facts about. skills is nil here (not the caller's skills arg): every ADK-native
		// agent's Toolsets already carries a SkillToolset, whose own ProcessRequest renders the roster - rendering it here too would duplicate it in every request (audit finding A4).
		return promptbuilder.Agent(name, desc, tools, nil, behaviour, grading, "")
	})
	cfg := llmagent.Config{
		Name:        name,
		Description: desc,
		Model:       m,
		InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
			return prompt(), nil
		},
		Tools:    tools,
		Toolsets: toolsets,
		Mode:     mode,
	}
	cfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{steerCallback(drain), HoistInstructionCallback(prompt)}
	return llmagent.New(cfg)
}

// HoistInstructionCallback moves own's text to the front of the assembled
// SystemInstruction, ahead of ADK's own prepended artifact/memory text.
func HoistInstructionCallback(own func() string) llmagent.BeforeModelCallback {
	return func(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		text := own()
		if text == "" || req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
			return nil, nil
		}
		parts := req.Config.SystemInstruction.Parts
		if len(parts) == 0 || parts[len(parts)-1] == nil {
			return nil, nil
		}
		last := parts[len(parts)-1]
		idx := strings.Index(last.Text, text)
		if idx <= 0 {
			return nil, nil // not present, or already leading
		}
		before := strings.TrimSuffix(last.Text[:idx], "\n\n")
		after := strings.TrimPrefix(last.Text[idx+len(text):], "\n\n")
		hoisted := text
		if before != "" {
			hoisted += "\n\n" + before
		}
		if after != "" {
			hoisted += "\n\n" + after
		}
		last.Text = hoisted
		return nil, nil
	}
}

// steerCallback delivers a message queued against a RUNNING node on the round's
// next model call. Without it a steer waits for the next gate boundary, which
// for a long native round is minutes away or never (#1029).
func steerCallback(drain func() string) llmagent.BeforeModelCallback {
	// Peeked text stays pending until the gate drains it, so without this the
	// same steer would be re-injected on every model call of the round.
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if drain == nil || req == nil {
			return nil, nil
		}
		q := strings.TrimSpace(drain())
		if q == "" {
			// Empty means the gate drained the queue, so anything after this is
			// a NEW steer - including the same words sent again because the
			// first appeared to do nothing.
			mu.Lock()
			clear(seen)
			mu.Unlock()
			return nil, nil
		}
		mu.Lock()
		dup := seen[q]
		seen[q] = true
		mu.Unlock()
		if dup {
			return nil, nil
		}
		req.Contents = append(req.Contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: q}},
		})
		return nil, nil
	}
}
