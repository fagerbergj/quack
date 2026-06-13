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
	// judgeMaxTokens bounds the judge's JSON verdict. Larger than before to
	// accommodate per-criterion reasons in the G-Eval response structure.
	judgeMaxTokens = 2048

	// judgeInstruction drives G-Eval-style scoring: the judge reasons through
	// each named criterion (reason first, then score) before computing the
	// overall. Reason-before-score eliminates round-number clustering and
	// holistic averaging bias; per-criterion scores let the caller enforce
	// hard caps in code regardless of what the judge computes.
	//
	// The example explicitly names all five keys inside criteria AND shows
	// score/passed/feedback at the top level — the most common model mistake
	// is nesting those three inside criteria instead of at the outer level.
	judgeInstruction = "You are an independent, skeptical judge. You did not write the answer. " +
		"Work through each named criterion in the rubric. " +
		"For each criterion write a one-sentence reason, then assign a score 0.0–1.0 using the rubric's scoring anchors. " +
		"Apply any hard caps stated in the rubric, then report the capped mean as the top-level score. " +
		"Respond with ONLY a valid JSON object. " +
		"IMPORTANT: score, passed, and feedback are TOP-LEVEL keys — do NOT place them inside criteria.\n" +
		"{\n" +
		"  \"criteria\": {\n" +
		"    \"grounded\":             {\"reason\": \"...\", \"score\": 0.0},\n" +
		"    \"no_fabrication\":       {\"reason\": \"...\", \"score\": 0.0},\n" +
		"    \"answers_question\":     {\"reason\": \"...\", \"score\": 0.0},\n" +
		"    \"internally_consistent\":{\"reason\": \"...\", \"score\": 0.0},\n" +
		"    \"cites_sources\":        {\"reason\": \"...\", \"score\": 0.0}\n" +
		"  },\n" +
		"  \"score\": 0.0,\n" +
		"  \"passed\": false,\n" +
		"  \"feedback\": \"what to fix\"\n" +
		"}"
)

// criterionScore is the judge's per-criterion assessment in a G-Eval verdict.
type criterionScore struct {
	Reason string  `json:"reason,omitempty"`
	Score  float64 `json:"score"`
}

// verdict is the judge's structured score for one round. When Criteria is
// populated (G-Eval mode), parseVerdict recomputes Score from the criterion
// averages and enforces hard caps in code rather than trusting the judge's
// holistic value.
type verdict struct {
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Score    float64                   `json:"score"`
	Passed   bool                      `json:"passed"`
	Feedback string                    `json:"feedback"`
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
// corrected answer. feedback is the judge's verdict for a post-judge revision
// pass; when empty (the pre-judge self-refine) no reviewer section is added.
func buildCritiqueContent(constitution, rubric string, question *genai.Content, draft string, act workerActivity, feedback string) *genai.Content {
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
	if strings.TrimSpace(feedback) != "" {
		sb.WriteString("An independent reviewer judged your draft below the bar and asked you to address this feedback specifically:\n")
		sb.WriteString(feedback)
		sb.WriteString("\n\n")
	}
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

// parseVerdict reads the judge's JSON, tolerating a ```json fenced block.
//
// It handles two known model failure modes:
//   - Truncated JSON: the model emits score/passed/feedback inside the criteria
//     object, leaving the outer object unclosed. We try appending one or two
//     closing braces before giving up.
//   - Misplaced top-level fields: after brace repair, score/passed/feedback
//     appear as keys inside criteria (not valid criterionScore objects). We
//     skip non-object entries and recover feedback/passed from them directly.
//
// When per-criterion scores are present (G-Eval mode) the overall score is
// recomputed from the criterion average with hard caps applied in code,
// overriding the judge's holistic value. The final score is clamped [0,1].
func parseVerdict(raw string) (verdict, error) {
	s := strings.TrimSpace(raw)
	// Strip any prefix before the first '{' (e.g. ```json fences).
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[i:]
	}

	// Intermediate type: criteria values are kept as raw JSON so we can
	// tolerate non-object entries (misplaced score/passed/feedback).
	type rawVerdict struct {
		Criteria map[string]json.RawMessage `json:"criteria,omitempty"`
		Score    float64                    `json:"score"`
		Passed   bool                       `json:"passed"`
		Feedback string                     `json:"feedback"`
	}

	// Use a Decoder (not Unmarshal) so it stops after the first complete JSON
	// object and ignores any trailing content — including a duplicated blob.
	var rv rawVerdict
	var parsed bool
	var lastErr error
	for _, suffix := range []string{"", "}", "}}"} {
		dec := json.NewDecoder(strings.NewReader(s + suffix))
		if err := dec.Decode(&rv); err == nil {
			parsed = true
			break
		} else {
			lastErr = err
		}
	}
	if !parsed {
		return verdict{}, fmt.Errorf("vetting: parse judge verdict %q: %w", raw, lastErr)
	}

	feedback := rv.Feedback
	if feedback == "None" || feedback == "null" || feedback == "N/A" {
		feedback = ""
	}
	v := verdict{Score: rv.Score, Passed: rv.Passed, Feedback: feedback}

	// Decode per-criterion entries, skipping non-object values. When score,
	// passed, or feedback ended up inside criteria, recover them explicitly.
	for name, entry := range rv.Criteria {
		var cs criterionScore
		if err := json.Unmarshal(entry, &cs); err != nil {
			switch name {
			case "feedback":
				json.Unmarshal(entry, &v.Feedback) //nolint:errcheck
			case "passed":
				json.Unmarshal(entry, &v.Passed) //nolint:errcheck
			}
			continue
		}
		if v.Criteria == nil {
			v.Criteria = make(map[string]criterionScore)
		}
		v.Criteria[name] = cs
	}

	// G-Eval aggregation: recompute score from per-criterion values when present.
	if len(v.Criteria) > 0 {
		var sum float64
		for _, c := range v.Criteria {
			sum += c.Score
		}
		avg := sum / float64(len(v.Criteria))

		// Hard cap: zero citations → score ≤ 0.40 regardless of other criteria.
		// Using < 0.05 rather than == 0 to tolerate floating-point imprecision.
		if cs, ok := v.Criteria["cites_sources"]; ok && cs.Score < 0.05 {
			if avg > 0.40 {
				avg = 0.40
			}
		}
		v.Score = avg
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
