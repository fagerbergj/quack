package serve

import (
	"context"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// modelIntentClassifier adapts a model.LLM to github.IntentClassifier: one
// prompt in, the model's raw text out. Backed by the SAME judge model the
// trust gate already runs (gates.judge) - it's deliberately co-resident with
// the workers on this deployment, so classification costs no model swap.
type modelIntentClassifier struct {
	model model.LLM
}

func (c *modelIntentClassifier) Classify(ctx context.Context, prompt string) (string, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: prompt}}}},
	}
	var out strings.Builder
	for resp, err := range c.model.GenerateContent(ctx, req, false) {
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
