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
