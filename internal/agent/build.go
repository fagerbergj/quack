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

// Build turns a loaded bundle into a runnable ADK llmagent, given its model,
// selected built-in tools, and optional ADK toolsets. Context compaction is
// wired separately, at the runner (see internal/agent/a2a.go's Serve).
// memoryGuidance (bundle's memory.md, M6) is appended to the behaviour layer
// only for memory-participating agents; skills is the agent's declared skill
// scope (promptbuilder.Agent); grading is the pre-rendered trust-gate
// contract (promptbuilder.GradingFacts), "" when ungated or judge-less.
func Build(b *Bundle, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, skills []*skill.Frontmatter, grading string, drain func() string) (adkagent.Agent, error) {
	return build(b, m, tools, toolsets, memoryGuidance, skills, grading, "", drain)
}

// BuildChat is Build with the agent's delegation mode PINNED to ModeChat at
// construction, for agents that run as a runner's ROOT over a multi-turn
// session (e.g. the advisor). Pinning matters beyond semantics: runner.Run
// force-sets an unset mode to ModeChat with an unsynchronized check-then-write
// on the shared agent - a data race when concurrent consults hit it at once.
// A pre-set mode turns that write into a pure read. Workers keep Build's
// unset mode, defaulting to single-turn task mode inside a workflow
// AgentNode, which is what the gate wants.
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
		// "" workspace: native bundles are never a coding agent (those run
		// as external ACP subprocesses - see internal/serve's ACP branch),
		// so there is no sandboxed clone/toolchain to state facts about.
		// skills is nil here (not the caller's skills arg): every ADK-native
		// agent's Toolsets already carries a SkillToolset, whose own
		// ProcessRequest renders the roster - rendering it here too would
		// duplicate it in every request (audit finding A4).
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
	cfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{steerCallback(drain)}
	return llmagent.New(cfg)
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
