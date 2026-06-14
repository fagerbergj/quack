package agent

import (
	"log"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
)

// Session compaction — when a long agentic loop fills the model's context slot,
// summarize the OLDER turns into a compact briefing and continue from it, so the
// worker keeps its findings (and their URLs) instead of overflowing and dying.
// Modelled on opencode's summarize tier (sst/opencode session/compaction.ts):
// keep the most recent turns verbatim ("tail"), summarize everything before
// ("head"). We do it in a BeforeModelCallback because the overflow builds up
// INSIDE one agent loop (between tool steps), which is where this hook fires.
const (
	// compactionContextTokens approximates the worker's per-request context slot
	// (llama-server --parallel 2 over -c 131072 ⇒ 65536 tokens/slot). If you change
	// --parallel or -c such that the per-slot size changes, change this.
	compactionContextTokens = 65536
	// compactionMargin leaves room under the slot for the model's own output plus
	// a safety buffer; compaction triggers once a request crosses it.
	compactionMargin = MaxOutputTokens + 6000
	// compactionTailTokens is how much recent context is kept verbatim; anything
	// older than this is summarized.
	compactionTailTokens = 24000
	// compactionSummaryMaxTokens caps the generated briefing.
	compactionSummaryMaxTokens = 1800
	// compactionInstruction preserves the specifics the citation gate needs.
	compactionInstruction = "You are compacting an in-progress research session to save context. Summarize everything found SO FAR into a compact briefing the agent can continue from without re-reading: the key findings, every exact figure (price, rating, date, time, address, name), and the source URL each fact came from. Preserve specifics and URLs verbatim — they will be cited. Output only the briefing, no preamble."
)

// compactionCallback returns a BeforeModelCallback that summarizes the older part
// of a long session before it overflows the context slot. It keeps the first
// message (the task) and a recent tail of turns verbatim, summarizes the middle
// via summarizer, and folds the briefing into the task message. On any
// uncertainty — no safe split point, empty/failed summary — it leaves the request
// untouched rather than risk corrupting the message sequence.
func compactionCallback(summarizer model.LLM) llmagent.BeforeModelCallback {
	return func(cbctx adkagent.CallbackContext, req *model.LLMRequest) (*model.LLMResponse, error) {
		contents := req.Contents
		if len(contents) < 4 {
			return nil, nil
		}
		if estimateTokens(contents) < compactionContextTokens-compactionMargin {
			return nil, nil
		}
		// Find the oldest "safe" tail boundary — a content with NO FunctionResponse,
		// so the tail can't start with a tool result whose call we summarized away
		// (an orphan the model API would reject) — that still keeps the tail within
		// budget.
		split := len(contents)
		tokens := 0
		for i := len(contents) - 1; i >= 1; i-- {
			tokens += contentChars(contents[i]) / 4
			if tokens > compactionTailTokens {
				break
			}
			if !hasFunctionResponse(contents[i]) {
				split = i
			}
		}
		if split <= 1 || split >= len(contents) {
			return nil, nil // no safe, useful split
		}
		summary, err := summarizeHead(cbctx, summarizer, contents[1:split])
		if err != nil || strings.TrimSpace(summary) == "" {
			log.Printf("compaction: skipped (summary unavailable: %v)", err)
			return nil, nil
		}
		// Fold the briefing into the task message (keeps a single leading user turn
		// → clean alternation with the tail), then keep the recent tail verbatim.
		task := *contents[0]
		task.Parts = append(append([]*genai.Part{}, contents[0].Parts...),
			&genai.Part{Text: "\n\n[Earlier research this session, summarized to save context]\n" + summary})
		newContents := append([]*genai.Content{&task}, contents[split:]...)
		log.Printf("compaction: summarized %d→%d contents (~%d→~%d tokens)", len(contents), len(newContents), estimateTokens(contents), estimateTokens(newContents))
		req.Contents = newContents
		return nil, nil
	}
}

// summarizeHead renders the head contents to text and asks summarizer for a
// fact/URL-preserving briefing. It's a raw model call (not an agent run), so it
// doesn't recurse back into this callback.
func summarizeHead(ctx adkagent.CallbackContext, m model.LLM, head []*genai.Content) (string, error) {
	var sb strings.Builder
	for _, c := range head {
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			}
			if p.FunctionResponse != nil {
				if r, ok := p.FunctionResponse.Response["result"].(string); ok && r != "" {
					sb.WriteString(r)
					sb.WriteByte('\n')
				}
			}
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", nil
	}
	return inference.Generate(ctx, m, &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: text}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: compactionInstruction}}},
			MaxOutputTokens:   compactionSummaryMaxTokens,
		},
	})
}

// estimateTokens approximates the token count of contents (~4 chars/token).
func estimateTokens(contents []*genai.Content) int {
	chars := 0
	for _, c := range contents {
		chars += contentChars(c)
	}
	return chars / 4
}

// contentChars sums the text-bearing characters of a content (answer/reasoning
// text plus string tool args and tool results — the parts that dominate size).
func contentChars(c *genai.Content) int {
	if c == nil {
		return 0
	}
	n := 0
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		n += len(p.Text)
		if p.FunctionResponse != nil {
			for _, v := range p.FunctionResponse.Response {
				if s, ok := v.(string); ok {
					n += len(s)
				}
			}
		}
		if p.FunctionCall != nil {
			for _, v := range p.FunctionCall.Args {
				if s, ok := v.(string); ok {
					n += len(s)
				}
			}
		}
	}
	return n
}

func hasFunctionResponse(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}
