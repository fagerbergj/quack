package stream

import "strings"

// thinkOpen/thinkClose are the reasoning delimiters qwen3.x emits into content
// when llama.cpp's reasoning parser doesn't capture the block (the opening tag
// never reaches it, so it can't route the block to reasoning_content).
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// StripThinking removes a model's reasoning block from text that should hold
// only the final answer. Two leak shapes:
//   - Closed: "<think>…</think>answer" (or a bare leading "</think>answer") -
//     drop up to and including the first </think>.
//   - UNCLOSED: "<think>…" with no </think> (budget hit, or stream ended
//     mid-think) - everything from <think> on is reasoning, so drop it all;
//     requiring a closing tag would leak the whole block.
//
// No markers ⇒ returned unchanged. An entirely-unclosed answer becomes ""
// - callers treat that as "no answer" and recover.
func StripThinking(s string) string {
	if i := strings.Index(s, thinkClose); i >= 0 {
		return strings.TrimSpace(s[i+len(thinkClose):])
	}
	if i := strings.Index(s, thinkOpen); i >= 0 {
		// Unclosed: keep only what came before the (never-closed) block.
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
