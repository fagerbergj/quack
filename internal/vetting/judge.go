package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/inference"
)

const (
	// judgeMaxTokens bounds the judge's JSON verdict.
	judgeMaxTokens = 256

	// judgeInstruction drives a single-pass verdict: score + pass/fail + one
	// line of feedback. No per-criterion breakdown — faster and sufficient for
	// a binary gate.
	judgeInstruction = "You are an independent, skeptical judge. You did not write the answer. " +
		"Score the answer against the rubric on a scale of 0.0–1.0. " +
		"Respond with ONLY a valid JSON object — no other text.\n" +
		"{\"score\": 0.0, \"passed\": false, \"feedback\": \"one sentence on the biggest gap, or empty string if none\"}"

	reviseInstruction = "You are revising your answer to address a reviewer's feedback. " +
		"Output ONLY the improved answer text — no preamble, no commentary."
)

// verdict is the judge's structured score for one round.
type verdict struct {
	Score    float64 `json:"score"`
	Passed   bool    `json:"passed"`
	Feedback string  `json:"feedback"`
}

// runJudge scores answer against the constitution + rubric using the judge model.
// constitution provides the global principles; rubric provides the per-agent
// scoring criteria; fetchedURLs is the set of URLs the worker called web_fetch
// on — used to pre-verify cited links before the judge sees the answer.
// onThinking receives streaming thinking tokens as they arrive (nil to discard).
func runJudge(ctx context.Context, m model.LLM, constitution, rubric string, question *genai.Content, answer string, fetchedURLs map[string]fetchRecord, onThinking func(string)) (verdict, error) {
	var sb strings.Builder
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Scoring rubric:\n")
	sb.WriteString(rubric)
	if section := buildSourceVerification(answer, fetchedURLs); section != "" {
		sb.WriteString("\n\n")
		sb.WriteString(section)
	}
	sb.WriteString("\n\nUser's question:\n")
	sb.WriteString(questionText(question))
	sb.WriteString("\n\nAnswer to judge:\n")
	sb.WriteString(answer)
	raw, err := generateStream(ctx, m, judgeInstruction, sb.String(), true, onThinking)
	if err != nil {
		return verdict{}, err
	}
	return parseVerdict(raw)
}

// markdownLinkRe extracts inline Markdown link targets: [text](https://…)
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\((https?://[^)\s]+)\)`)

// buildSourceVerification returns a prompt section listing which cited URLs were
// successfully fetched by the worker (with a content sample) and which were not.
// Returns "" when the agent made no web_fetch calls (non-web agents) or no
// Markdown links appear in the answer, so the section is omitted entirely.
//
// The content sample lets the judge spot-check whether cited claims are actually
// supported by the pages the worker read — not just whether the URL exists.
func buildSourceVerification(answer string, fetched map[string]fetchRecord) string {
	if len(fetched) == 0 {
		return ""
	}
	matches := markdownLinkRe.FindAllStringSubmatch(answer, -1)
	if len(matches) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(matches))
	type citedURL struct {
		url    string
		record fetchRecord
		ok     bool
	}
	var cited []citedURL
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		u := strings.TrimSpace(m[1])
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		rec, ok := fetched[u]
		cited = append(cited, citedURL{url: u, record: rec, ok: ok})
	}

	var sb strings.Builder
	sb.WriteString("Source verification (system-checked — whether cited URLs were successfully fetched this session):\n")
	var hasUnverified bool
	for _, c := range cited {
		if c.ok {
			sb.WriteString("  ✓ fetched: ")
			sb.WriteString(c.url)
			if c.record.sample != "" {
				sb.WriteString("\n    content sample: \"")
				sb.WriteString(c.record.sample)
				sb.WriteString("\"")
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString("  ✗ NOT fetched: ")
			sb.WriteString(c.url)
			sb.WriteString("\n")
			hasUnverified = true
		}
	}
	if hasUnverified {
		sb.WriteString("A URL marked NOT fetched was cited but never successfully retrieved — treat it as fabricated when scoring `no_fabrication` and `cites_sources`.")
	}
	return sb.String()
}

// buildCritiqueContent constructs the user message for the agentic self-refine
// pass. The worker receives its own draft alongside the rubric and a directive
// to use its tools to fix any gaps — fetching missing sources, verifying
// claims, retrieving URLs it cited but did not read — then output only the
// corrected answer.
func buildCritiqueContent(constitution, rubric string, question *genai.Content, draft string, act workerActivity) *genai.Content {
	var sb strings.Builder
	sb.WriteString("You previously drafted an answer to the question below. " +
		"Review it critically against the scoring criteria. " +
		"Use your tools to fix any gaps — fetch missing sources, verify claims, retrieve URLs you cited but did not read. " +
		"Then output only the corrected answer with no preamble or commentary. " +
		"If the draft already meets all criteria, output it unchanged.\n\n")
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Scoring criteria:\n")
	sb.WriteString(rubric)
	sb.WriteString("\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(section)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Original question:\n")
	sb.WriteString(questionText(question))
	sb.WriteString("\n\nYour draft:\n")
	sb.WriteString(draft)
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// buildActivitySection returns a prompt section summarising what retrieval the
// worker performed. An empty return means no retrieval happened (non-web agent
// or a session where all fetches failed) — the section is omitted entirely.
func buildActivitySection(act workerActivity) string {
	if len(act.searches) == 0 && len(act.fetched) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Session activity (retrieval the worker performed — do not contradict this):\n")
	for _, q := range act.searches {
		sb.WriteString("  • web_search: \"")
		sb.WriteString(q)
		sb.WriteString("\"\n")
	}
	for u := range act.fetched {
		sb.WriteString("  • web_fetch: ")
		sb.WriteString(u)
		sb.WriteString("\n")
	}
	return sb.String()
}

// revise asks the worker's own model to improve the answer given judge feedback.
// onThinking receives streaming thinking tokens as they arrive (nil to discard).
func revise(ctx context.Context, m model.LLM, question *genai.Content, answer, feedback string, onThinking func(string)) (string, error) {
	prompt := "Question:\n" + questionText(question) +
		"\n\nYour previous answer:\n" + answer +
		"\n\nReviewer feedback to address:\n" + feedback
	return generateStream(ctx, m, reviseInstruction, prompt, false, onThinking)
}

// generateStream runs one model round-trip in streaming mode and returns the
// concatenated non-thought text. Thinking chunks are passed to onThinking as
// they arrive (nil disables the callback). jsonMode requests a JSON object
// response (honored by the openai adapter).
func generateStream(ctx context.Context, m model.LLM, system, user string, jsonMode bool, onThinking func(string)) (string, error) {
	cfg := &genai.GenerateContentConfig{MaxOutputTokens: quackagent.MaxOutputTokens}
	if system != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: system}}}
	}
	if jsonMode {
		cfg.ResponseMIMEType = "application/json"
		cfg.MaxOutputTokens = judgeMaxTokens
	}
	return inference.StreamGenerate(ctx, m, &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: user}}}},
		Config:   cfg,
	}, onThinking)
}

// parseVerdict reads the judge's JSON, tolerating a ```json fenced block and
// truncated output (tries appending one or two closing braces before giving up).
func parseVerdict(raw string) (verdict, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[i:]
	}

	var v verdict
	var lastErr error
	for _, suffix := range []string{"", "}", "}}"} {
		dec := json.NewDecoder(strings.NewReader(s + suffix))
		if err := dec.Decode(&v); err == nil {
			break
		} else {
			lastErr = err
			v = verdict{}
		}
	}
	if lastErr != nil && v.Score == 0 && !v.Passed && v.Feedback == "" {
		return verdict{}, fmt.Errorf("vetting: parse judge verdict %q: %w", raw, lastErr)
	}

	if v.Feedback == "None" || v.Feedback == "null" || v.Feedback == "N/A" {
		v.Feedback = ""
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
