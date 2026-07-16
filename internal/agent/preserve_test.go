package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// The retained tail must hold SEVERAL recent tool round-trips, not collapse
// to one file at a time.
//
// THE ROOT CAUSE (code-mode dogfood, 2026-07-13): a fixed 8k-token preserve
// cap (opencode's MAX_PRESERVE_RECENT_TOKENS, ported without opencode's
// 200k+-context premise) left room for exactly one Go source file (~6-7k
// tokens) in the tail, so every new read evicted the previous one and the
// model reread the same file eight times. The ADK port replaces that
// token-fraction cap with a count-based EventRetentionSize (default 20
// request contents); this pins the same invariant against the new mechanism.
func TestRetentionHoldsSeveralFilesNotJustOne(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// threshold = 120_000 - 20_000 = 100_000: small enough to force
	// compaction, generous enough that the default retention's tail (10
	// pairs) still fits without the backstop clamp ladder engaging.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 120_000, Enabled: true})

	const sourceFileChars = 28_000 // ~7k tokens, a normal Go source file
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 15; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", sourceFileChars))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("test precondition broken: compaction did not fire (calls=%d)", llm.calls)
	}

	intact := strings.Repeat("x", sourceFileChars)
	held := 0
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if fr := p.FunctionResponse; fr != nil {
				if got, _ := fr.Response["result"].(string); got == intact {
					held++
				}
			}
		}
	}
	if held < 3 {
		t.Fatalf("tail holds %d full source files; want at least 3 — an implementer must hold the file it is "+
			"editing, the file it is calling, and its test at the same time", held)
	}
}
