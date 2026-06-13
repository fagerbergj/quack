package vetting

import (
	"context"
	"encoding/json"
	"fmt"
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
		"Verify it agentically: use web_search and web_fetch to independently re-fetch the URLs the answer cites and confirm that each material claim is actually supported by its source. A cited URL that does not load, or does not support the claim it is attached to, is a fabrication. " +
		"Then work through each named criterion in the rubric. For each, reason in one or two sentences, then assign a score 0.0–1.0 using the rubric's scoring anchors. " +
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
func buildJudgePrompt(constitution, rubric string, question *genai.Content, answer string, fetchedURLs map[string]fetchRecord) string {
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
		sb.WriteString("\nThis is a hint from the worker's session — verify it yourself with your tools; do not take it on faith.")
	}
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
func runJudgeAgent(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer string, fetchedURLs map[string]fetchRecord, emit func(*genai.Part) bool) (verdict, error) {
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

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: buildJudgePrompt(cfg.Constitution, cfg.Rubric, question, answer, fetchedURLs)}}}

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
