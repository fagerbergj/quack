package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
)

const (
	// judgeMaxTokens bounds the judge's JSON verdict; it is small.
	judgeMaxTokens = 1024

	judgeInstruction = "You are an independent, skeptical judge. You did not write the answer. " +
		"Score how well the answer satisfies the rubric, being strict about unsupported or fabricated claims. " +
		"Respond with ONLY a JSON object: {\"score\": <number 0..1>, \"passed\": <bool>, \"feedback\": \"<what specifically must improve>\"}."

	selfRefineInstruction = "You are revising your own draft answer before anyone else sees it. " +
		"Critique it for errors, unsupported claims, and gaps, then output ONLY the improved answer text — no preamble, no commentary. " +
		"If the draft is already good, output it unchanged."

	reviseInstruction = "You are revising your answer to address a reviewer's feedback. " +
		"Output ONLY the improved answer text — no preamble, no commentary."
)

// verdict is the judge's structured score for one round.
type verdict struct {
	Score    float64 `json:"score"`
	Passed   bool    `json:"passed"`
	Feedback string  `json:"feedback"`
}

// runJudge scores answer against the rubric using the independent judge model.
func runJudge(ctx context.Context, m model.LLM, rubric string, question *genai.Content, answer string) (verdict, error) {
	prompt := "Rubric:\n" + rubric +
		"\n\nUser's question:\n" + questionText(question) +
		"\n\nAnswer to judge:\n" + answer
	raw, err := generate(ctx, m, judgeInstruction, prompt, true)
	if err != nil {
		return verdict{}, err
	}
	return parseVerdict(raw)
}

// selfRefine asks the worker's own model to critique and improve its draft.
func selfRefine(ctx context.Context, m model.LLM, question *genai.Content, answer string) (string, error) {
	prompt := "Question:\n" + questionText(question) + "\n\nDraft answer:\n" + answer
	return generate(ctx, m, selfRefineInstruction, prompt, false)
}

// revise asks the worker's own model to improve the answer given judge feedback.
func revise(ctx context.Context, m model.LLM, question *genai.Content, answer, feedback string) (string, error) {
	prompt := "Question:\n" + questionText(question) +
		"\n\nYour previous answer:\n" + answer +
		"\n\nReviewer feedback to address:\n" + feedback
	return generate(ctx, m, reviseInstruction, prompt, false)
}

// generate runs one model round-trip and returns the concatenated non-thought
// text. jsonMode requests a JSON object response (honored by the openai adapter).
func generate(ctx context.Context, m model.LLM, system, user string, jsonMode bool) (string, error) {
	cfg := &genai.GenerateContentConfig{MaxOutputTokens: quackagent.MaxOutputTokens}
	if system != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: system}}}
	}
	if jsonMode {
		cfg.ResponseMIMEType = "application/json"
		cfg.MaxOutputTokens = judgeMaxTokens
	}
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: user}}}},
		Config:   cfg,
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
	return strings.TrimSpace(out.String()), nil
}

// parseVerdict reads the judge's JSON, tolerating a ```json fenced block, and
// clamps the score to [0,1].
func parseVerdict(raw string) (verdict, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	var v verdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return verdict{}, fmt.Errorf("vetting: parse judge verdict %q: %w", raw, err)
	}
	if v.Score < 0 {
		v.Score = 0
	}
	if v.Score > 1 {
		v.Score = 1
	}
	return v, nil
}

// questionText extracts the user's question text from the invocation's content.
func questionText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
