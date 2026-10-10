package agent

import (
	"context"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// Build turns a loaded bundle into a runnable ADK llmagent (compaction is wired at a2a.go's Serve). grading is the
// pre-rendered trust-gate contract, "" when ungated or judge-less; wire adds callbacks such as tools.Hooks.Wire.
func Build(b *Bundle, prompts *artifactsrc.Pinned, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, grading string, drain func() string, meter *PromptMeter, wire ...func(*llmagent.Config)) (adkagent.Agent, error) {
	name, desc := b.Card.Name, b.Card.Description
	if prompts == nil {
		prompts = b.PinPrompt(nil)
	}
	// Every input below is fixed once Build returns except today() and the pinned prompt, which only the gate moves.
	prompt := promptbuilder.CacheByDay(
		func(context.Context) string { return prompts.Get().VersionID },
		func(context.Context) string {
			// Native bundles never get a workspace (coding agents run over ACP). Tools and skills are omitted:
			// their declarations and the SkillToolset already reach the model, so listing them would duplicate them.
			layer := BehaviourLayer(strings.TrimSpace(prompts.Get().Body), memoryGuidance)
			return promptbuilder.Agent(name, desc, nil, false, layer, grading, "")
		})
	cfg := llmagent.Config{
		Name:        name,
		Description: desc,
		Model:       m,
		InstructionProvider: func(rc adkagent.ReadonlyContext) (string, error) {
			return prompt(rc), nil
		},
		Tools:    tools,
		Toolsets: toolsets,
	}
	cfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{steerCallback(drain)}
	for _, w := range wire {
		w(&cfg)
	}
	if meter != nil {
		meter.wire(&cfg)
	}
	return llmagent.New(cfg)
}

// BuildChat is Build for a runner-root agent with no steer queue or prompt meter. Mode stays unset: adk
// v2.4.0 resolves a root to chat in ctx without writing the shared agent (older versions raced on that write).
func BuildChat(b *Bundle, prompts *artifactsrc.Pinned, m model.LLM, tools []tool.Tool, toolsets []tool.Toolset, memoryGuidance string, grading string) (adkagent.Agent, error) {
	return Build(b, prompts, m, tools, toolsets, memoryGuidance, grading, nil, nil)
}

// BehaviourLayer is the bundle's prompt.md followed by its memory guidance when the agent has one.
func BehaviourLayer(prompt, memoryGuidance string) string {
	if g := strings.TrimSpace(memoryGuidance); g != "" {
		return prompt + "\n\n" + g
	}
	return prompt
}

// steerCallback delivers a steer queued against a RUNNING node on the round's next model call,
// rather than at the next gate boundary, which for a long native round may be minutes away or never.
func steerCallback(drain func() string) llmagent.BeforeModelCallback {
	// Peeked text stays pending until the gate drains it; without this the same steer repeats every model call.
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if drain == nil || req == nil {
			return nil, nil
		}
		q := strings.TrimSpace(drain())
		if q == "" {
			// The gate drained the queue, so the same words sent again later are a NEW steer.
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
