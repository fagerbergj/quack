package vetting

import (
	"context"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// Adversarial verify (#370): after the primary judge passes a criterion, an
// independent skeptic tries to REFUTE it before the gate trusts it. This is
// the second half of #359's ground-truth judge — the primary judge already
// has repo read access (see judgeReadToolsClause); this stage spends that
// same access adversarially, on a NARROW set of findings rather than
// re-litigating every criterion.

// submitSkepticVerdictTool is the structured-termination tool a skeptic calls
// to record its refute/survive call and end its run — mirrors judge.go's
// submit_verdict pattern.
const submitSkepticVerdictTool = "submit_skeptic_verdict"

// skepticVerdictArgs is the schema a skeptic fills when calling
// submitSkepticVerdictTool.
type skepticVerdictArgs struct {
	Refuted bool   `json:"refuted"`
	Reason  string `json:"reason"`
}

// skepticInstruction is the skeptic's whole system prompt: given ONE finding
// the primary judge already scored as passing, try to tear it down. Defaults
// to refuted on any doubt — the asymmetry the issue calls for ("defaulting to
// 'refuted' when uncertain").
const skepticInstruction = "You are an adversarial skeptic. Another judge already scored ONE finding below as PASSING; your only job is to try to REFUTE it — actively look for reasons it is wrong, not reasons it might be right. " +
	"You did not write the answer and must not extend it good faith. If you have read-only workspace tools (read_file, list_dir, glob, grep), use them to check any checkable claim against the real files rather than trusting the finding's own reasoning — they reach the SAME workspace the worker used. " +
	"Default to REFUTED whenever you are uncertain, cannot verify the claim, or find anything that does not clearly hold up — the finding only SURVIVES when you are confident it is correct. " +
	"Call submit_skeptic_verdict exactly once with `refuted` (bool) and `reason` (one or two sentences naming what you found, whichever way you decided)."

// NewSkepticFactory returns a SkepticFactory built the same way judge.go's
// NewJudgeFactory is: a fresh agentic skeptic per round, closing over
// skepticModel and the SAME read-only workspace tools the primary judge holds
// (so a skeptic checking a repo claim reaches the worker's real clone too —
// no separate clone, exactly like the primary judge). Pass nil/empty
// readTools for a tool-less skeptic that argues from the answer text alone.
func NewSkepticFactory(skepticModel model.LLM, readTools []tool.Tool) SkepticFactory {
	return func(sink *skepticVerdict) (adkagent.Agent, error) {
		submit, err := newSubmitSkepticVerdictTool(sink)
		if err != nil {
			return nil, err
		}
		skepticTools := make([]tool.Tool, 0, len(readTools)+1)
		skepticTools = append(skepticTools, readTools...)
		skepticTools = append(skepticTools, submit)
		return llmagent.New(llmagent.Config{
			Name:        "skeptic",
			Description: "adversarial refuter of one judge finding",
			Model:       skepticModel,
			Instruction: skepticInstruction,
			Tools:       skepticTools,
		})
	}
}

// skepticVerdict is one skeptic's refute/survive call on a single finding.
type skepticVerdict struct {
	Refuted bool
	Reason  string
}

// SkepticFactory builds a fresh, isolated skeptic agent bound to sink — one
// per round, mirroring JudgeFactory.
type SkepticFactory func(sink *skepticVerdict) (adkagent.Agent, error)

func newSubmitSkepticVerdictTool(sink *skepticVerdict) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        submitSkepticVerdictTool,
		Description: "Record whether you refuted the finding, and end the evaluation. Call this exactly once.",
	}, func(ctx adkagent.Context, args skepticVerdictArgs) (map[string]any, error) {
		*sink = skepticVerdict{Refuted: args.Refuted, Reason: args.Reason}
		ctx.Actions().Escalate = true
		ctx.Actions().SkipSummarization = true
		return map[string]any{"recorded": true}, nil
	})
}

// loadBearing reports whether a criterion is worth spending an adversarial
// pass on: judge-authored (not one of foldDeterministic's own checks, which
// are already ground truth — refuting code's own verdict is pointless) and
// currently PASSING (a criterion already scored below threshold is already
// caught; the adversarial pass exists to catch a plausible-but-wrong PASS).
func loadBearing(c criterionScore, threshold float64) bool {
	return !strings.HasPrefix(c.Reason, "deterministic:") && c.Score >= threshold
}

// buildSkepticPrompt is the user message handed to one skeptic: the finding
// under test (criterion name + the primary judge's own reasoning for it), plus
// enough of the original exchange to evaluate it against.
func buildSkepticPrompt(criterion string, c criterionScore, question *genai.Content, answer string, act workerActivity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Finding to refute — criterion %q, scored %.0f/10 as PASSING by the primary judge, reasoning:\n%s\n\n", criterion, c.Score*10, strings.TrimSpace(c.Reason))
	sb.WriteString("Original question:\n")
	sb.WriteString(boundExcerpt(questionText(question), maxOriginalQuestionChars))
	sb.WriteString("\n\nAnswer being evaluated:\n")
	sb.WriteString(boundExcerpt(answer, maxPreviousAnswerChars))
	if ws := buildWorkspaceSection(act); ws != "" {
		sb.WriteString("\n\n")
		sb.WriteString(ws)
	}
	sb.WriteString("\n\nDoes this specific finding actually hold up? Call submit_skeptic_verdict now.")
	return sb.String()
}

// runSkepticRound runs one isolated skeptic agent (its own in-memory session,
// like runJudgeRound) and returns its refute/survive call. Any run error, or a
// skeptic that ends without calling submit_skeptic_verdict, is treated as
// REFUTED — the gate fails closed on adversarial verification exactly as it
// does on a primary judge error (see RunGatedRefine's judge-error path).
func runSkepticRound(ctx context.Context, factory SkepticFactory, criterion string, c criterionScore, question *genai.Content, answer string, act workerActivity, emit func(*genai.Part) bool) skepticVerdict {
	var sink skepticVerdict
	skepticAgent, err := factory(&sink)
	if err != nil {
		return skepticVerdict{Refuted: true, Reason: fmt.Sprintf("skeptic build failed, defaulting to refuted: %v", err)}
	}
	sr, err := runner.New(runner.Config{
		AppName: "quack-skeptic", Agent: skepticAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return skepticVerdict{Refuted: true, Reason: fmt.Sprintf("skeptic runner failed, defaulting to refuted: %v", err)}
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: buildSkepticPrompt(criterion, c, question, answer, act)}}}
	var submitted bool
	for ev, rerr := range sr.Run(ctx, "skeptic", "verdict", content, adkagent.RunConfig{}) {
		if rerr != nil {
			return skepticVerdict{Refuted: true, Reason: fmt.Sprintf("skeptic run failed, defaulting to refuted: %v", rerr)}
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p == nil:
				continue
			case p.FunctionCall != nil && p.FunctionCall.Name == submitSkepticVerdictTool:
				submitted = true
			case p.Thought && p.Text != "" && emit != nil:
				if !emit(stream.ThinkingPart(p.Text)) {
					return skepticVerdict{Refuted: true, Reason: "consumer disconnected mid-round, defaulting to refuted"}
				}
			}
		}
	}
	if !submitted {
		return skepticVerdict{Refuted: true, Reason: "skeptic ended without a verdict, defaulting to refuted"}
	}
	return sink
}

// adversarialVerify runs cfg.SkepticRounds independent skeptics against each
// load-bearing criterion in v (see loadBearing) and downgrades any finding a
// MAJORITY refute — killing the criterion (score 0, weakest-link) rather than
// merely trimming it, since a majority-refuted finding is exactly the
// plausible-but-wrong case the gate must not ship. A tie (survives==refutes)
// favours the primary judge's original PASS: adversarial verify exists to
// catch a confident wrong pass, not to demand unanimous re-confirmation of
// every close call. No-op when cfg.Skeptic is unset or SkepticRounds <= 0.
func adversarialVerify(ctx context.Context, cfg Config, question *genai.Content, answer string, act workerActivity, v verdict, emit func(*genai.Part) bool) verdict {
	if cfg.Skeptic == nil || cfg.SkepticRounds <= 0 || len(v.Criteria) == 0 {
		return v
	}
	for name, c := range v.Criteria {
		if !loadBearing(c, cfg.Threshold) {
			continue
		}
		refuted := 0
		var reasons []string
		for i := 0; i < cfg.SkepticRounds; i++ {
			sv := runSkepticRound(ctx, cfg.Skeptic, name, c, question, answer, act, emit)
			if sv.Refuted {
				refuted++
				if strings.TrimSpace(sv.Reason) != "" {
					reasons = append(reasons, sv.Reason)
				}
			}
		}
		if refuted*2 > cfg.SkepticRounds { // strict majority
			v.Criteria[name] = criterionScore{
				Score: 0,
				Reason: fmt.Sprintf("adversarial verify: %d/%d skeptics refuted this finding — %s",
					refuted, cfg.SkepticRounds, strings.Join(reasons, "; ")),
			}
		}
	}
	return aggregateVerdict(v)
}
