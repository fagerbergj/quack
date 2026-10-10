package serve

import (
	"context"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// classifyWithModel backs Host.Classify on the trust gate's judge model, which is co-resident with the
// workers, so classification costs no model swap.
func classifyWithModel(ctx context.Context, m model.LLM, prompt string) (string, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: prompt}}}},
	}
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
	return out.String(), nil
}
