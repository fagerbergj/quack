package agent

import (
	"context"
	"log"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
)

// Session compaction — when a long agentic loop fills the model's context window,
// summarize the OLDER turns into a compact briefing and continue from it, so the
// worker keeps its findings (and their URLs) instead of overflowing and dying.
//
// Modelled on opencode's summarize tier (sst/opencode session/compaction.ts):
// keep the most recent turns verbatim ("tail"), summarize everything before
// ("head"). It is triggered REACTIVELY — when the model server rejects a request
// for exceeding the context window (see reactivecompact.go) — NOT by a token
// estimate. We tried estimating (chars/token, then the model's own tokenizer) and
// kept getting it wrong on dense content; the server's 400 is the ground truth for
// "too big," so we compact on that and retry.
const (
	// compactionTailTokens bounds how much recent context is kept verbatim; older
	// turns are summarized. Sized with a conservative chars/2 (~a dense 2
	// chars/token) so the compacted request comfortably fits the window on retry.
	compactionTailTokens = 24000
	// compactionSummaryMaxTokens caps the generated briefing.
	compactionSummaryMaxTokens = 1800
	// maxSummarizeInputChars bounds the text fed to ONE summary call so the call
	// itself can't overflow the window — the crux of the problem: we compact
	// BECAUSE we're at the limit, so the summarizer must never see the whole
	// over-limit context at once. 60000 chars stays under the 65536-token slot even
	// at a pathological 1 char/token (60000 + instruction + 1800 output reserve).
	maxSummarizeInputChars = 60000
	// maxSummarizeDepth bounds the reduce recursion (summaries-of-summaries) so a
	// gigantic head can't loop forever.
	maxSummarizeDepth = 3
	// compactionInstruction preserves the specifics the citation gate needs.
	compactionInstruction = "You are compacting an in-progress research session to save context. Summarize everything found SO FAR into a compact briefing the agent can continue from without re-reading: the key findings, every exact figure (price, rating, date, time, address, name), and the source URL each fact came from. Preserve specifics and URLs verbatim — they will be cited. Output only the briefing, no preamble."
)

// compactContents summarizes the older turns of an over-long session and returns a
// shorter content slice: the first message (the task) with the summary folded in,
// then the recent tail verbatim. ok is false when it can't safely compact (too
// short, no safe split point, or the summary call failed) so the caller leaves the
// request unchanged. summarizer MUST be a raw model (not the compacting wrapper)
// so the summary call can't recurse back through the overflow path.
func compactContents(ctx context.Context, summarizer model.LLM, contents []*genai.Content) ([]*genai.Content, bool) {
	if len(contents) < 4 {
		return contents, false
	}
	// Find the oldest "safe" tail boundary — a content with NO FunctionResponse, so
	// the tail can't start with a tool result whose call we summarized away (an
	// orphan the model API rejects) — that keeps the tail within budget.
	split := len(contents)
	tokens := 0
	for i := len(contents) - 1; i >= 1; i-- {
		tokens += contentChars(contents[i]) / 2 // conservative chars/token (dense web content ~2.3)
		if tokens > compactionTailTokens {
			break
		}
		if !hasFunctionResponse(contents[i]) {
			split = i
		}
	}
	if split <= 1 || split >= len(contents) {
		log.Printf("compaction: over budget but no safe split point — cannot compact")
		return contents, false
	}
	summary, err := summarizeHead(ctx, summarizer, contents[1:split])
	if err != nil || strings.TrimSpace(summary) == "" {
		log.Printf("compaction: skipped (summary unavailable: %v)", err)
		return contents, false
	}
	// Fold the briefing into the task message (keeps a single leading user turn →
	// clean alternation with the tail), then keep the recent tail verbatim.
	task := *contents[0]
	task.Parts = append(append([]*genai.Part{}, contents[0].Parts...),
		&genai.Part{Text: "\n\n[Earlier research this session, summarized to save context]\n" + summary})
	newContents := append([]*genai.Content{&task}, contents[split:]...)
	log.Printf("compaction: summarized %d→%d contents", len(contents), len(newContents))
	return newContents, true
}

// summarizeHead renders the head contents to text and summarizes them into a
// fact/URL-preserving briefing. It is raw model call(s) (not agent runs), so it
// doesn't recurse back into the compaction path.
func summarizeHead(ctx context.Context, m model.LLM, head []*genai.Content) (string, error) {
	text := strings.TrimSpace(renderHeadText(head))
	if text == "" {
		return "", nil
	}
	return summarizeText(ctx, m, text, 0)
}

// summarizeText summarizes text that may ITSELF exceed the context window — the
// whole point of the question "how can we summarize if we're at the limit?". It
// splits the text into window-safe chunks and summarizes each (map); if the
// combined briefing is still too big, it summarizes THAT (reduce), bounded by
// maxSummarizeDepth. No single model call ever sees more than maxSummarizeInputChars.
func summarizeText(ctx context.Context, m model.LLM, text string, depth int) (string, error) {
	if len(text) <= maxSummarizeInputChars {
		return summarizeOnce(ctx, m, text)
	}
	if depth >= maxSummarizeDepth {
		// Can't reduce further; summarize the leading window as a best effort rather
		// than fail compaction entirely.
		return summarizeOnce(ctx, m, text[:safeCut(text, maxSummarizeInputChars)])
	}
	chunks := chunkByChars(text, maxSummarizeInputChars)
	parts := make([]string, 0, len(chunks))
	for _, ch := range chunks {
		s, err := summarizeOnce(ctx, m, ch)
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}
	joined := strings.Join(parts, "\n\n")
	if len(joined) <= maxSummarizeInputChars {
		return joined, nil
	}
	return summarizeText(ctx, m, joined, depth+1) // reduce
}

// summarizeOnce is a single window-safe summary call, memoized by chunk content so
// the stable (immutable) chunks of a growing head are summarized exactly once and
// reused on later over-budget turns — only the trailing chunk is ever redone.
func summarizeOnce(ctx context.Context, m model.LLM, text string) (string, error) {
	key := chunkKey(text)
	if cached, ok := summaryCache.get(key); ok {
		return cached, nil
	}
	out, err := inference.Generate(ctx, m, &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: text}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: compactionInstruction}}},
			MaxOutputTokens:   compactionSummaryMaxTokens,
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) != "" {
		summaryCache.put(key, out)
	}
	return out, nil
}

// renderHeadText concatenates the readable text of the head contents (turn text +
// tool-result "result" strings) for summarization.
func renderHeadText(head []*genai.Content) string {
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
	return sb.String()
}

// chunkByChars splits s into pieces of at most max bytes, preferring to cut at a
// newline (and never mid-rune) so each chunk is valid text the summarizer can read.
func chunkByChars(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var chunks []string
	for len(s) > max {
		cut := strings.LastIndexByte(s[:max], '\n')
		if cut <= 0 {
			cut = safeCut(s, max)
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
	}
	if strings.TrimSpace(s) != "" {
		chunks = append(chunks, s)
	}
	return chunks
}

// safeCut returns a cut index ≤ max that doesn't split a UTF-8 rune.
func safeCut(s string, max int) int {
	if max >= len(s) {
		return len(s)
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return max // pathological; accept the byte cut
	}
	return cut
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
