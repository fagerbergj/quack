package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// preloadInstructions matches ADK's preloadmemorytool wording so recalled memory
// reads the same to the model.
const preloadInstructions = `The following content is from your previous conversations with the user.
They may be useful for answering the user's current query.
<PAST_CONVERSATIONS>
%s
</PAST_CONVERSATIONS>`

// oncePreload is a drop-in for ADK's preloadmemorytool that recalls memory
// ONCE per invocation - at the first model step - instead of on every step
// of the tool-call loop. ADK documents preload as a turn-boundary action,
// but the v1.4.0 flow runs every tool's ProcessRequest on every runOneStep,
// so the stock tool re-searches and re-injects on each tool call. Within one
// invocation the query is constant, so those repeats are pure waste and, as
// the store grows, risk context rot from low-relevance hits. We inject the
// recalled block once; it stays in the system instruction for the rest of
// the loop, invisible to the model like ADK's version.
type oncePreload struct{}

// NewPreload returns a once-per-invocation preload-memory processor.
func NewPreload() tool.Tool { return oncePreload{} }

func (oncePreload) Name() string { return "preload_memory" }
func (oncePreload) Description() string {
	return "Preloads relevant memory once at the start of the turn."
}
func (oncePreload) IsLongRunning() bool { return false }

func (oncePreload) ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error {
	uc := ctx.UserContent()
	if uc == nil || len(uc.Parts) == 0 || uc.Parts[0] == nil || uc.Parts[0].Text == "" {
		return nil
	}
	query := uc.Parts[0].Text

	// First-step guard. ToolContext exposes no session, but req.Contents (built by an
	// earlier request processor) holds the history. The orchestrator's session is
	// long-lived, so it also holds prior turns - anchor on the LATEST real user message
	// (the current turn) and skip if the model already produced output after it. On the
	// first step there is none, so recall runs exactly once per turn; later tool-loop
	// steps short-circuit.
	fs := firstStep(req.Contents)
	slog.Default().Debug("preload", "component", "memory", "contents", len(req.Contents), "first_step", fs)
	if !fs {
		return nil
	}

	resp, err := ctx.SearchMemory(ctx, query)
	if err != nil {
		return fmt.Errorf("preload memory search failed: %w", err)
	}
	if resp == nil || len(resp.Memories) == 0 {
		return nil
	}
	text := formatMemories(resp.Memories)
	if text == "" {
		return nil
	}
	appendInstruction(req, fmt.Sprintf(preloadInstructions, text))
	return nil
}

// firstStep reports whether the model has not yet acted on the current turn, given
// the request's content history. It finds the LATEST real user message - a user-role
// content that carries text (function responses are also user-role but textless) -
// and returns true only if nothing follows it. The latest user message is the current
// turn even in a long-lived session, so this works without matching exact query text
// (which doesn't survive the A2A boundary byte-for-byte). Empty/no-user-message →
// assume first step so recall still runs.
func firstStep(contents []*genai.Content) bool {
	last := -1
	for i, c := range contents {
		if c != nil && c.Role == genai.RoleUser && hasText(c) {
			last = i
		}
	}
	if last < 0 {
		return true
	}
	return last == len(contents)-1
}

func hasText(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			return true
		}
	}
	return false
}

func formatMemories(memories []adkmemory.Entry) string {
	var lines []string
	for _, m := range memories {
		t := extractText(m)
		if t == "" {
			continue
		}
		if !m.Timestamp.IsZero() {
			lines = append(lines, "Time: "+m.Timestamp.Format(time.RFC3339))
		}
		if m.Author != "" {
			t = m.Author + ": " + t
		}
		lines = append(lines, t)
	}
	return strings.Join(lines, "\n")
}

func extractText(m adkmemory.Entry) string {
	if m.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// appendInstruction replicates ADK's internal utils.AppendInstructions (which we
// can't import): append the recalled block to the request's system instruction.
func appendInstruction(r *model.LLMRequest, inst string) {
	if r.Config == nil {
		r.Config = &genai.GenerateContentConfig{}
	}
	si := r.Config.SystemInstruction
	if si == nil {
		r.Config.SystemInstruction = genai.NewContentFromText(inst, genai.RoleUser)
		return
	}
	if n := len(si.Parts); n > 0 && si.Parts[n-1].Text != "" {
		si.Parts[n-1].Text += "\n\n" + inst
		return
	}
	si.Parts = append(si.Parts, genai.NewPartFromText(inst))
}

// recallInstructions frames gate-side recall for an EXTERNAL (ACP) worker,
// whose prompt quack assembles itself - the same role preloadInstructions
// plays for a native agent's system instruction, worded for a coding agent.
const recallInstructions = `The following notes were remembered from previous runs on this repository /
task family. Use them instead of re-deriving what they already answer; they may
be stale, so verify anything load-bearing against the code itself.
<MEMORY>
%s
</MEMORY>`

// Recall returns the formatted recall block for query under sc - the gate-side
// twin of preload_memory for external workers (vetting injects it at the front
// of the worker prompt). "" when the store is nil, nothing matches, or the
// embedder is unavailable: recall is best-effort and bounded (Store.recall),
// so it can never fail or hang a node.
func (s *Store) Recall(ctx context.Context, sc Scope, query string) string {
	if s == nil {
		return ""
	}
	resp, err := s.recall(ctx, sc.Buckets(), query)
	if err != nil || resp == nil {
		return ""
	}
	text := formatMemories(resp.Memories)
	if text == "" {
		return ""
	}
	return fmt.Sprintf(recallInstructions, text)
}
