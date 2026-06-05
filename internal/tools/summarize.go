package tools

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

const (
	summarizeMaxTokens   = 1024
	summarizeInstruction = "Summarize the provided text into a faithful, compact summary. Preserve the key facts, names, numbers, dates, and source URLs. Include only information that appears in the text itself."
)

type summarizeArgs struct {
	Text  string `json:"text"`
	Focus string `json:"focus,omitempty"`
}

// newSummarize builds the summarize tool, which calls a model to condense text.
// It lets the researcher compress fetched pages before reasoning over them.
func newSummarize(d Deps) (tool.Tool, error) {
	if d.Summarizer == nil {
		return nil, fmt.Errorf("summarize requires a model")
	}
	m := d.Summarizer

	return functiontool.New[summarizeArgs, string](
		functiontool.Config{
			Name:        "summarize",
			Description: "Summarize a long block of text, optionally focused on a question or topic. Returns a compact, faithful summary.",
		},
		func(tc tool.Context, a summarizeArgs) (string, error) {
			return summarizeText(tc, m, a.Text, a.Focus)
		},
	)
}

// summarizeText runs one model round-trip to condense text, optionally focused.
func summarizeText(ctx context.Context, m model.LLM, text, focus string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("summarize: empty text")
	}
	prompt := text
	if f := strings.TrimSpace(focus); f != "" {
		prompt = "Focus on: " + f + "\n\nText:\n" + text
	}

	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: prompt}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: summarizeInstruction}}},
			MaxOutputTokens:   summarizeMaxTokens,
		},
	}

	var out strings.Builder
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", fmt.Errorf("summarize: %w", err)
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
