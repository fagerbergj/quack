package stream

import "strings"

// qwen3.x reasoning delimiters that leak into content when llama.cpp's parser misses the opening tag.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// StripThinking drops a leaked reasoning block: up to the first </think>, or everything from an unclosed
// <think> (budget hit). An entirely-unclosed answer becomes "", which callers treat as no answer.
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
