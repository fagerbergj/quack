package stream

import "strings"

// thinkOpen/thinkClose are the reasoning delimiters qwen3.x emits into content
// when llama.cpp's reasoning parser doesn't capture the block (the opening tag
// never reaches it, so it can't route the block to reasoning_content).
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// StripThinking removes a model's reasoning block from text that is supposed to
// hold only the final answer. It handles the two leak shapes we actually see
// from qwen3.6 (per hermes-webui#2152):
//
//   - Closed block — "<think>…</think>answer" or a bare leading "</think>answer":
//     drop everything up to and including the first </think>, keep the rest.
//   - UNCLOSED block — "<think>…" with no </think> (the model hit its token /
//     reasoning budget, or the stream ended, before closing): everything from
//     <think> on is reasoning, so drop it. A strip that requires the closing tag
//     (our old logic) would leak the whole block — this is the title-leak bug.
//
// Text with no reasoning markers is returned unchanged (only outer newlines
// trimmed). When the answer was entirely an unclosed reasoning block, the result
// is empty — callers treat that as "no answer" and recover.
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
