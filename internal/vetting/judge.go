package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/stream"
)

const (
	// submitVerdictTool is the name of the structured-termination tool the
	// agentic judge calls to record its verdict and end its run.
	submitVerdictTool = "submit_verdict"

	// defaultJudgeMaxIterations bounds the judge's agentic tool loop (model
	// turns per round) when Config.JudgeMaxIterations is unset.
	defaultJudgeMaxIterations = 6

	// judgeAgentBehaviour is the behaviour layer of the agentic judge's system
	// prompt (promptbuilder.Judge wraps it with identity, tools, and environment
	// layers, exactly like a specialist agent's prompt.md). Unlike the old
	// one-shot scorer it tells the judge to verify the answer with its own tools
	// (re-fetching cited URLs, checking claims) before scoring, then to terminate
	// by calling submit_verdict — never by emitting JSON text. Per-criterion
	// reason-before-score (G-Eval) keeps the scoring disciplined; the caller
	// re-derives the overall score with hard caps in aggregateVerdict.
	judgeAgentBehaviour = "You did NOT write the answer being evaluated, and you must not trust its assertions. " +
		"You have no tools — judge the answer on its own merits against the rubric: whether it actually answers the question, stays internally consistent, and whether anything stated with specificity (names, prices, numbers, dates) reads as invented or unsupported. Do NOT try to verify which URLs were fetched — citation backing is checked separately by deterministic code, so score `cites_sources` only on whether claims carry followable links at all, not on whether you think a URL is real. " +
		"Work through each named criterion in the rubric. For each, reason in one or two sentences, then assign a score 0.0–1.0 using the rubric's scoring anchors. " +
		"When — and only when — you have evaluated every criterion, call the submit_verdict tool exactly once with: `score` (the rubric mean after applying any hard caps stated in the rubric), `criteria` (an object mapping each criterion name to {reason, score}), and `feedback` (concrete, actionable notes on what to fix; empty when the answer passes). " +
		"Do NOT write the verdict as prose or JSON in your reply — calling submit_verdict is the only way to finish. " +
		"The five criteria are: grounded, no_fabrication, answers_question, internally_consistent, cites_sources."
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

// JudgeFactory builds a fresh agentic judge bound to sink: when the judge calls
// the submit_verdict tool, its arguments are written into sink. A new judge is
// built per round so each round's submit_verdict binds a clean sink. The factory
// closes over the judge model and the judge's verification tools (web_search,
// web_fetch); see NewJudgeFactory.
type JudgeFactory func(sink *verdict) (adkagent.Agent, error)

// NewJudgeFactory returns a JudgeFactory that builds the agentic judge as an ADK
// llmagent with judgeModel, the supplied verification webTools (web_search,
// web_fetch), and a per-round submit_verdict tool bound to the caller's sink.
func NewJudgeFactory(judgeModel model.LLM, webTools []tool.Tool) JudgeFactory {
	return func(sink *verdict) (adkagent.Agent, error) {
		submit, err := newSubmitVerdictTool(sink)
		if err != nil {
			return nil, err
		}
		judgeTools := make([]tool.Tool, 0, len(webTools)+1)
		judgeTools = append(judgeTools, webTools...)
		judgeTools = append(judgeTools, submit)
		return llmagent.New(llmagent.Config{
			Name:        "judge",
			Description: "independent skeptical verifier",
			Model:       judgeModel,
			InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
				return promptbuilder.Judge(judgeTools, judgeAgentBehaviour), nil
			},
			Tools: judgeTools,
			GenerateContentConfig: &genai.GenerateContentConfig{
				MaxOutputTokens: quackagent.MaxOutputTokens,
			},
		})
	}
}

// verdictArgs is the schema the judge fills when calling submit_verdict. Only
// score is required; criteria and feedback are optional so a terse judge call
// still validates (aggregateVerdict tolerates absent criteria).
type verdictArgs struct {
	Score    float64                   `json:"score"`
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Feedback string                    `json:"feedback,omitempty"`
}

// newSubmitVerdictTool builds the structured-termination tool. Its handler
// records the verdict into sink and escalates so the judge's run ends
// immediately (no further model turn), mirroring ADK's exitlooptool pattern.
func newSubmitVerdictTool(sink *verdict) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        submitVerdictTool,
		Description: "Record your final verdict and end the evaluation. Call this exactly once, after independently verifying the answer against every rubric criterion.",
	}, func(ctx tool.Context, args verdictArgs) (map[string]any, error) {
		*sink = verdict{Score: args.Score, Criteria: args.Criteria, Feedback: args.Feedback}
		ctx.Actions().Escalate = true
		ctx.Actions().SkipSummarization = true
		return map[string]any{"recorded": true}, nil
	})
}

// buildJudgePrompt is the user message handed to the agentic judge: the
// constitution + rubric, the system-checked source verification hint (which URLs
// the worker actually fetched), the question, and the answer to judge.
func buildJudgePrompt(constitution, rubric string, question *genai.Content, answer string) string {
	var sb strings.Builder
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Scoring rubric:\n")
	sb.WriteString(rubric)
	sb.WriteString("\n\nUser's question:\n")
	sb.WriteString(questionText(question))
	sb.WriteString("\n\nAnswer to judge:\n")
	sb.WriteString(answer)
	return sb.String()
}

// runJudgeAgent runs one agentic judge round in its own isolated runner +
// in-memory session, so the judge's tool calls never touch the worker's session.
// emit receives display copies of the judge's thinking and tool activity (the
// caller authors them so the worker's revision context can filter them out); it
// returns false when the consumer has disconnected, which aborts the round.
//
// The verdict is captured structurally via submit_verdict (sink). If the judge
// ends without calling it, runJudgeAgent falls back to parsing any text it
// emitted, and failing that returns an error so the gate degrades gracefully.
func runJudgeAgent(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer string, emit func(*genai.Part) bool) (verdict, error) {
	var sink verdict
	judgeAgent, err := factory(&sink)
	if err != nil {
		return verdict{}, fmt.Errorf("vetting: build judge agent: %w", err)
	}
	jr, err := runner.New(runner.Config{
		AppName:           "quack-judge",
		Agent:             judgeAgent,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return verdict{}, fmt.Errorf("vetting: judge runner: %w", err)
	}

	maxIters := cfg.JudgeMaxIterations
	if maxIters <= 0 {
		maxIters = defaultJudgeMaxIterations
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: buildJudgePrompt(cfg.Constitution, cfg.Rubric, question, answer)}}}

	var (
		submitted bool
		turns     int
		accum     strings.Builder
	)
	for ev, err := range jr.Run(runCtx, "judge", "verdict", content, adkagent.RunConfig{}) {
		if err != nil {
			return verdict{}, err
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil && p.FunctionCall.Name == submitVerdictTool:
				submitted = true // handler runs as part of this call; sink is populated
			case p.FunctionResponse != nil && p.FunctionResponse.Name == submitVerdictTool:
				// suppress from display
			case p.Thought && p.Text != "":
				if !emit(stream.ThinkingPart(p.Text)) {
					return verdict{}, context.Canceled
				}
			case p.FunctionCall != nil:
				if !emit(&genai.Part{FunctionCall: p.FunctionCall}) {
					return verdict{}, context.Canceled
				}
			case p.FunctionResponse != nil:
				if !emit(&genai.Part{FunctionResponse: p.FunctionResponse}) {
					return verdict{}, context.Canceled
				}
			case p.Text != "":
				// The local model emits reasoning as plain text rather than Thought
				// parts; surface it as thinking and keep it for the text fallback.
				accum.WriteString(p.Text)
				if !emit(stream.ThinkingPart(p.Text)) {
					return verdict{}, context.Canceled
				}
			}
		}
		if ev.TurnComplete {
			turns++
		}
		// Safety cap: a judge that never calls submit_verdict can't loop forever.
		if turns > maxIters {
			cancel()
			break
		}
	}

	if submitted {
		return aggregateVerdict(sink), nil
	}
	// Fallback: judge ended without a structured verdict. Try its text, else fail.
	if v, perr := parseVerdict(accum.String()); perr == nil {
		return v, nil
	}
	return verdict{}, fmt.Errorf("vetting: judge ended without a verdict")
}

// markdownLinkRe extracts inline Markdown link targets: [text](https://…)
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\((https?://[^)\s]+)\)`)

// citationScore deterministically grades how well each cited URL in the answer
// is backed by what the worker actually retrieved this session — no model
// involved, so it can't "reason wrong" about a string match the way a small
// judge model does. Each cited URL is scored in layers:
//
//	exact URL fetched   → 1.00   (the worker read this exact page)
//	exact URL searched  → 0.75   (this exact URL appeared in search results)
//	same host fetched   → 0.50   (a different page on this host was fetched)
//	same host searched  → 0.25   (the host showed up in search results)
//	neither             → 0.00   (the worker never encountered this URL or host)
//
// URLs are normalized (lowercased scheme+host, fragment dropped, trailing slash
// trimmed) before matching so cosmetic differences don't cost points. The
// returned score is the mean across distinct cited URLs; details carries the
// per-URL breakdown for logging/feedback. ok is false when the answer cites no
// URLs (caller decides how to treat an uncited answer).
func citationScore(answer string, act workerActivity) (score float64, details []citationDetail, ok bool) {
	// No retrieval recorded (a non-web agent like the synthesizer, which re-cites
	// URLs from its upstream inputs) → we can't grade backing, so don't override;
	// leave the model's cites_sources judgment in place.
	if len(act.fetched) == 0 && len(act.seen) == 0 {
		return 0, nil, false
	}
	fetchedURL, fetchedHost := normalizedSets(keysOf(act.fetched))
	seenURL, seenHost := normalizedSets(keysOf(act.seen))

	dedup := make(map[string]struct{})
	var sum float64
	for _, m := range markdownLinkRe.FindAllStringSubmatch(answer, -1) {
		if len(m) < 2 {
			continue
		}
		norm, host := normalizeURL(m[1])
		if norm == "" {
			continue
		}
		if _, dup := dedup[norm]; dup {
			continue
		}
		dedup[norm] = struct{}{}

		var s float64
		switch {
		case fetchedURL[norm]:
			s = 1.00
		case seenURL[norm]:
			s = 0.75
		case host != "" && fetchedHost[host]:
			s = 0.50
		case host != "" && seenHost[host]:
			s = 0.25
		default:
			s = 0.00
		}
		details = append(details, citationDetail{url: m[1], score: s})
		sum += s
	}
	if len(details) == 0 {
		return 0, nil, false
	}
	return sum / float64(len(details)), details, true
}

// citationDetail is one cited URL's deterministic backing score.
type citationDetail struct {
	url   string
	score float64
}

// normalizeURL lowercases the scheme+host, drops the fragment (#anchor), and
// trims a trailing slash from the path, returning the normalized URL and its
// host. On a parse failure it falls back to the trimmed raw string with no host.
func normalizeURL(raw string) (norm, host string) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/"), ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String(), u.Host
}

// normalizedSets returns the set of normalized URLs and the set of their hosts.
func normalizedSets(urls []string) (urlSet, hostSet map[string]bool) {
	urlSet = make(map[string]bool, len(urls))
	hostSet = make(map[string]bool, len(urls))
	for _, u := range urls {
		n, h := normalizeURL(u)
		if n != "" {
			urlSet[n] = true
		}
		if h != "" {
			hostSet[h] = true
		}
	}
	return urlSet, hostSet
}

// keysOf returns the keys of a string-keyed map (works for both fetched and seen).
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lengthScore is a deterministic length gate. For now it only catches a
// genuinely empty answer (0.0); any non-empty answer scores 1.0. A semantic
// "is this long enough for THIS question" judgment isn't possible
// deterministically — code can't know the depth a given question needs, and a
// fixed char floor would wrongly penalize legitimately concise answers — so we
// deliberately keep it to the 0-length check. The single function makes it easy
// to extend later (e.g. truncation detection) without touching the gate wiring.
func lengthScore(answer string) float64 {
	if strings.TrimSpace(answer) == "" {
		return 0.0
	}
	return 1.0
}

// unbackedCitations lists cited URLs that scored 0.0 — never retrieved this
// session in any form (the worker neither fetched nor searched them or their
// host). These are the clear, deterministically-fixable defect the cheap
// deterministic stage revises away.
func unbackedCitations(details []citationDetail) []string {
	var out []string
	for _, d := range details {
		if d.score == 0 {
			out = append(out, d.url)
		}
	}
	return out
}

// weakCitations lists cited URLs that scored below 0.5 (no exact-URL or
// same-host fetch backs them), for revision feedback. Returns "" when every
// citation is well-backed.
func weakCitations(details []citationDetail) string {
	var weak []string
	for _, d := range details {
		if d.score < 0.5 {
			weak = append(weak, fmt.Sprintf("%s (%.2f)", d.url, d.score))
		}
	}
	return strings.Join(weak, ", ")
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

// buildRevisionContent constructs the user message for the agentic, session-
// continuing revision: the worker is re-invoked (continuing its own session and
// tools) to address the judge's feedback, then output only the corrected answer.
// It mirrors buildCritiqueContent but is driven by the reviewer's feedback rather
// than a generic self-critique.
func buildRevisionContent(constitution string, question *genai.Content, answer, feedback string, act workerActivity) *genai.Content {
	var sb strings.Builder
	sb.WriteString("An independent reviewer evaluated your previous answer and it must be improved before it can be returned. " +
		"Address the reviewer's feedback below: use your tools to fix the gaps — re-fetch and verify sources, correct or remove unsupported claims, add missing citations. " +
		"Then output only the corrected answer with no preamble or commentary.\n\n")
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Reviewer feedback to address:\n")
	sb.WriteString(feedback)
	sb.WriteString("\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(section)
		sb.WriteString("\n")
	}
	sb.WriteString("Original question:\n")
	sb.WriteString(questionText(question))
	sb.WriteString("\n\nYour previous answer:\n")
	sb.WriteString(answer)
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// buildFinalizeContent asks the worker to write its final answer when round 0
// ended without one. It continues the worker's session (tool results already in
// context), so it only needs the directive plus the original question.
func buildFinalizeContent(question *genai.Content, act workerActivity) *genai.Content {
	var sb strings.Builder
	sb.WriteString("A response of 0 length was received. If you have finished your research, " +
		"do not call any more tools — just write your complete response again now, using everything " +
		"you found above. If you are not done with your research, there was likely an error: please " +
		"make sure you close all of your reasoning/thinking and tool-call blocks and continue " +
		"your research. Output only the answer with no preamble or commentary.\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(section)
		sb.WriteString("\n")
	}
	sb.WriteString("Question:\n")
	sb.WriteString(questionText(question))
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// aggregateVerdict re-derives the overall score from per-criterion values when
// present (G-Eval mode), applies hard caps in code (overriding the judge's
// holistic value), and clamps the final score to [0,1]. Used for both the
// structured submit_verdict path and the parseVerdict text fallback.
func aggregateVerdict(v verdict) verdict {
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
	return v
}

// parseVerdict reads the judge's JSON, tolerating a ```json fenced block. It is
// the fallback path for when the agentic judge ends without calling the
// submit_verdict tool but leaves a parseable verdict in its text.
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

	return aggregateVerdict(v), nil
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
