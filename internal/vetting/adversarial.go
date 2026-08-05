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

// Adversarial verify: independent skeptic tries to REFUTE passing criteria before the gate trusts them.

// submitSkepticVerdictTool: structured-termination tool for skeptic, mirrors submit_verdict.
const submitSkepticVerdictTool = "submit_skeptic_verdict"

// skepticVerdictArgs: schema for submitSkepticVerdictTool.
type skepticVerdictArgs struct {
	Refuted bool   `json:"refuted"`
	Reason  string `json:"reason"`
}

// skepticInstruction: try to refute ONE passing finding. Defaults to refuted on any doubt.
const skepticInstruction = "You are an adversarial skeptic. Another judge already scored ONE finding below as PASSING; your only job is to try to REFUTE it - actively look for reasons it is wrong, not reasons it might be right. " +
	"You did not write the answer and must not extend it good faith. If you have read-only workspace tools (read_file, list_dir, glob, grep), use them to check any checkable claim against the real files rather than trusting the finding's own reasoning - they reach the SAME workspace the worker used. " +
	"Default to REFUTED whenever you are uncertain, cannot verify the claim, or find anything that does not clearly hold up - the finding only SURVIVES when you are confident it is correct. " +
	"Call submit_skeptic_verdict exactly once with `refuted` (bool) and `reason` (one or two sentences naming what you found, whichever way you decided)."

// NewSkepticFactory: builds SkepticFactory same way as NewJudgeFactory, using same read-only tools.
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

// skepticVerdict: one skeptic's refute/survive call.
type skepticVerdict struct {
	Refuted bool
	Reason  string
}

// SkepticFactory: builds a fresh skeptic agent per round, mirroring JudgeFactory.
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

// loadBearing: judge-authored and passing (not ground-truth deterministic checks).
func loadBearing(c criterionScore, threshold float64) bool {
	return !strings.HasPrefix(c.Reason, "deterministic:") && c.Score >= threshold
}

// buildSkepticPrompt: the finding under test + original exchange context.
func buildSkepticPrompt(criterion string, c criterionScore, question *genai.Content, answer string, act workerActivity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Finding to refute - criterion %q, scored %.0f/10 as PASSING by the primary judge, reasoning:\n%s\n\n", criterion, c.Score*10, strings.TrimSpace(c.Reason))
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

// runSkepticRound: runs one isolated skeptic. Any error defaults to REFUTED (fails closed).
func runSkepticRound(ctx context.Context, factory SkepticFactory, maxIters int, criterion string, c criterionScore, question *genai.Content, answer string, act workerActivity, emit func(*genai.Part) bool) skepticVerdict {
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

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: buildSkepticPrompt(criterion, c, question, answer, act)}}}
	var (
		submitted bool
		turns     int
	)
	for ev, rerr := range sr.Run(runCtx, "skeptic", "verdict", content, adkagent.RunConfig{}) {
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
		if ev.TurnComplete {
			turns++
		}
		// Safety cap: a stuck skeptic must not stall sequential rounds.
		if turns > maxIters {
			cancel()
			break
		}
	}
	if !submitted {
		return skepticVerdict{Refuted: true, Reason: "skeptic ended without a verdict, defaulting to refuted"}
	}
	return sink
}

// adversarialVerify: kills criteria a MAJORITY of skeptics refute. Tie favours the primary judge.
func adversarialVerify(ctx context.Context, cfg Config, question *genai.Content, answer string, act workerActivity, v verdict, emit func(*genai.Part) bool) verdict {
	if cfg.Skeptic == nil || cfg.SkepticRounds <= 0 || len(v.Criteria) == 0 {
		return v
	}
	// Same turn cap as runJudgeRound.
	maxIters := cfg.JudgeMaxIterations
	if maxIters <= 0 {
		maxIters = defaultJudgeMaxIterations
	}
	for name, c := range v.Criteria {
		if !loadBearing(c, cfg.Threshold) {
			continue
		}
		refuted := 0
		var reasons []string
		for i := 0; i < cfg.SkepticRounds; i++ {
			sv := runSkepticRound(ctx, cfg.Skeptic, maxIters, name, c, question, answer, act, emit)
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
				Reason: fmt.Sprintf("adversarial verify: %d/%d skeptics refuted this finding - %s",
					refuted, cfg.SkepticRounds, strings.Join(reasons, "; ")),
			}
		}
	}
	return aggregateVerdict(v)
}
