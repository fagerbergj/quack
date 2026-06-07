package inference

import (
	"context"
	"strings"

	"google.golang.org/adk/model"
)

// Generate runs one model round-trip and returns the concatenated non-thought
// text. Errors from any streaming chunk abort the call immediately.
func Generate(ctx context.Context, m model.LLM, req *model.LLMRequest) (string, error) {
	var out strings.Builder
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", err
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p.Thought || p.Text == "" {
				continue
			}
			out.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// StreamGenerate runs one model round-trip using streaming mode so thinking
// tokens surface live. Thought parts are passed to onThinking as they arrive
// (nil disables the callback); non-thought text is accumulated and returned.
//
// Some OpenAI-compatible adapters emit each text delta as a partial response
// AND then re-emit the full accumulated text in the final TurnComplete response.
// To avoid doubling the output, text is only taken from partial responses once
// any partial text has been seen; the final TurnComplete text is used only when
// no partials arrived (i.e. a non-streaming adapter path).
func StreamGenerate(ctx context.Context, m model.LLM, req *model.LLMRequest, onThinking func(string)) (string, error) {
	var out strings.Builder
	var hasPartialText bool
	for resp, err := range m.GenerateContent(ctx, req, true) {
		if err != nil {
			return "", err
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p == nil {
				continue
			}
			if p.Thought && p.Text != "" {
				if onThinking != nil {
					onThinking(p.Text)
				}
				continue
			}
			if p.Text != "" {
				if resp.Partial {
					out.WriteString(p.Text)
					hasPartialText = true
				} else if !hasPartialText {
					// No partial deltas received — adapter sent a single complete
					// response; take its text.
					out.WriteString(p.Text)
				}
				// If hasPartialText && !resp.Partial: the final TurnComplete
				// re-emits the full text already accumulated from deltas — skip it.
			}
		}
	}
	return strings.TrimSpace(out.String()), nil
}
