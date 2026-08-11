package serve

import (
	"context"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// classifyWithModel is one free-text model round trip: prompt in, the
// model's raw text out. Backs Host.Classify, bound to the SAME judge model
// the trust gate already runs (gates.judge) - deliberately co-resident with
// the workers on this deployment, so classification costs no model swap.
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
