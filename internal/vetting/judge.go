package vetting

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// judgeScope: logger for gen_ai.evaluation.result ledger events.
const judgeScope = "quack.judge"

const (
	// submitVerdictTool: structured-termination tool for judge verdict.
	submitVerdictTool = "submit_verdict"

	// Cap on judge tool loops (model turns per round).
	defaultJudgeMaxIterations = 14

	// judgeBehaviour* compose the judge's behaviour prompt.
	judgeBehaviourHead = "You are the LAST line of defense before this answer ships. You did NOT write it, and you must not trust its assertions, its self-report of what it did, OR its inline citations - an answer's own claim that it verified something is not verification. If garbage or a fabricated claim ships past you, that is YOUR failure, not the worker's: score as an adversarial, skeptical verifier whose job is to catch what a confident, fluent, possibly-wrong answer wants you to wave through. "

	judgeNoToolsClause = "You have no tools. Judge the answer on its own merits against the rubric. "

	judgeReadToolsClause = "You have read-only workspace tools: read_file, list_dir, glob, grep. They reach the SAME clone/workspace the worker used (no separate clone spins up), so any specific, checkable claim the answer makes about THIS repo's code - a file exists, a function/struct/field/symbol, a config key, a control-flow path - is no longer a matter of plausibility. These tools are STRICTLY read-only: you cannot and must not modify, create, delete, or run anything in the workspace. " +
		"The clone root is your working root - use plain repo-relative paths (e.g. `frontend/src/pages/Chat.tsx`, `internal/foo.go`). NEVER use a leading slash or an absolute path (`/frontend`, `/workspace/...`) - those resolve ABOVE the clone and will not find the code. If a path isn't found, drop any leading slash and retry it repo-relative before concluding the claim is unverifiable. " +
		"Your read-tool calls are counted, and a PASS with zero reads is discarded and re-judged: nothing in such a verdict was verified. So before scoring any grounding/accuracy/correctness criterion, identify every load-bearing specific claim the answer makes about this repo and check each one yourself with grep/glob/read_file - the ledger summary and the answer's own account of what it read are not substitutes. An in-repo claim you have not verified that way is UNSUPPORTED, not grounded, however confident it reads; one that contradicts what the file actually shows is a fabrication regardless of any citation, and sinks the criterion it backs. " +
		"The workspace ledger below is evidence, not proof of diligence - treat it adversarially. If the answer makes claims about this repo's code but the ledger shows little or no local file reading (for example the worker web_fetched pages instead of using read_file/grep on the clone - a `web_fetch` entry hitting a repo host like raw.githubusercontent.com or api.github.com when read_file entries are sparse or absent is a RED FLAG, not a substitute), do not extend the benefit of the doubt: verify the claims yourself by reading the repo, and score the grounding/accuracy criteria harshly if you cannot confirm them. Read only what you need to reach a verdict - inspect the claimed files, do not spelunk the whole tree. For a pure-research answer with no in-repo claims you won't need these tools. "

	judgeSkillsClause = "You also have skill tools (list_skills, load_skill, load_skill_resource) - the same skills the worker could use. When it helps, load a relevant review or quality skill (for example a code-review skill like `ponytail-review`, or call list_skills to see what is available) so you can ground your quality assessment in the SAME principles the worker was told to follow, rather than principles baked into this prompt. This is OPTIONAL and bounded: use your judgment, do not load a skill on every case, load at most what you need to reach a verdict, and still finish with exactly one submit_verdict call. "

	judgeBehaviourTail = "If an image is attached to this message, you can see it - use it to directly verify any visual claims in the answer. If there is no image, judge on internal consistency and appropriate hedging only; do NOT penalise an answer merely because you cannot see the source. " +
		"Do NOT try to verify which URLs were fetched - citation backing is checked separately by deterministic code, so score `cites_sources` only on whether claims carry followable links at all, not on whether you think a URL is real. " +
		"CRITICAL - the leniency below is SCOPED, not a blanket pass for citations: it protects claims about LIVE WEB or EXTERNAL content you have no way to check from here (a fresh article, an external product, a fact outside this repo) and your own world knowledge is stale and incomplete, so NEVER treat such a claim as fabricated or ungrounded merely because you do not recognize it, it sounds new, or it postdates your training - an unfamiliar title, name, product, or event is NOT evidence of fabrication there. A specific is 'invented' only when the answer's OWN text is internally inconsistent or makes a precise claim it never supports, never because it conflicts with your memory. This leniency NEVER applies to claims about the workspace/repo: when you hold read tools, verify them per the mandatory instructions above - a citation there is a pointer to go check, not proof, and an unverified in-repo claim scores as unsupported even if it 'sounds right'; when you hold no tools, judge in-repo claims on internal consistency only, same as any other unverifiable claim, without extending web-content leniency to them. " +
		"Score EVERY criterion the rubric names - no more, no fewer. For each, reason in one or two sentences, then assign an INTEGER score from 0 to 10 using the rubric's scoring bands (10 = the criterion is fully met, 0 = total failure; use the intermediate values for partial quality - do not snap to 0, 5, or 10). Judge substance, not style: length and fluent prose earn no credit. Each criterion is an independent requirement: the answer's overall score is its WEAKEST criterion, so a single failing criterion sinks it - do not let a strong dimension excuse a failing one. " +
		"When - and only when - you have scored every criterion, call the submit_verdict tool exactly once with: `criteria` (an object mapping each criterion name to {reason, score}), `score` (a fallback the gate uses only if you submit no criteria - with criteria present it derives the overall score from them, so your per-criterion reasoning is the work that counts), and `feedback` (concrete, actionable notes naming the lowest-scoring criteria and what to fix; empty when the answer passes). " +
		"A FAILING criterion's `reason` is handed to the worker verbatim as its brief for the next attempt, so name the specific thing that failed - the claim, file, path, link, or command - never a restatement of the score. A reason that leaves the worker unable to tell WHICH item to fix has told it nothing. " +
		"submit_verdict is the only way to finish: a verdict written as prose or JSON in your reply is never read."
)

// judgeBehaviour: assembles the judge's behaviour prompt from tool-presence clauses.
func judgeBehaviour(hasReadTools, hasSkills bool) string {
	clause := judgeNoToolsClause
	if hasReadTools {
		clause = judgeReadToolsClause
	}
	skills := ""
	if hasSkills {
		skills = judgeSkillsClause
	}
	return judgeBehaviourHead + clause + skills + judgeBehaviourTail
}

// criterionScore: per-criterion assessment, normalised 0.0-1.0.
type criterionScore struct {
	Reason string  `json:"reason,omitempty"`
	Score  float64 `json:"score"`
	// Deterministic marks a code-owned criterion (set by mergeDeterministic from
	// computeDeterministicCriteria's provenance, never by the judge) - json:"-" so
	// it never appears in the judge's tool schema or gets round-tripped from its output.
	Deterministic bool `json:"-"`
}

// verdict: structured round score. Score is lowest criterion (weakest-link).
type verdict struct {
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Score    float64                   `json:"score"`
	Passed   bool                      `json:"passed"`
	Feedback string                    `json:"feedback"`
	Findings []findingVerdict          `json:"findings,omitempty"` // per-finding verification; "contradicted" folds into findingsGroundingCriterion

	// ChangedFiles* are set from changedFilesCoverage after the round, not by
	// the model - how much of the diff the judge actually saw (#779).
	ChangedFilesScored int `json:"changed_files_scored,omitempty"`
	ChangedFilesTotal  int `json:"changed_files_total,omitempty"`
}

// JudgeFactory: builds a fresh agentic judge per round, per-factory read-only tools, per-round readCounter.
// maxIters wires forcedVerdictCallback so the round's last allowed turn (or a repeated identical tool
// call) forces a text-only verdict instead of silently exhausting the budget (#853). maxOutputTokens
// caps the round's own reply tokens against a runaway generation loop; <= 0 leaves it uncapped (#889).
type JudgeFactory func(sink *verdict, maxIters, maxOutputTokens int) (adkagent.Agent, *readCounter, error)

// NewJudgeFactory: builds agentic judge with judgeModel, read-only tools, skillsets, and submit_verdict.
func NewJudgeFactory(judgeModel model.LLM, readTools []tool.Tool, skillsets []tool.Toolset) JudgeFactory {
	behaviour := judgeBehaviour(len(readTools) > 0, len(skillsets) > 0)
	return func(sink *verdict, maxIters, maxOutputTokens int) (adkagent.Agent, *readCounter, error) {
		submit, err := newSubmitVerdictTool(sink)
		if err != nil {
			return nil, nil, err
		}
		counted, reads := countReads(readTools)
		judgeTools := make([]tool.Tool, 0, len(readTools)+1)
		judgeTools = append(judgeTools, counted...)
		judgeTools = append(judgeTools, submit)
		a, err := llmagent.New(llmagent.Config{
			Name:        "judge",
			Description: "independent skeptical verifier",
			Model:       judgeModel,
			InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
				return promptbuilder.Judge(judgeTools, behaviour), nil
			},
			Tools:                 judgeTools,
			Toolsets:              skillsets,
			GenerateContentConfig: judgeGenConfig(maxOutputTokens),
			BeforeModelCallbacks:  []llmagent.BeforeModelCallback{forcedVerdictCallback(maxIters)},
		})
		return a, reads, err
	}
}

// judgeGenConfig caps a judge/skeptic/plan-judge round's own reply tokens - a
// verdict is a few hundred tokens of JSON, but an ungoverned round can decode
// tens of thousands looping (#889). <= 0 leaves the request uncapped.
func judgeGenConfig(maxOutputTokens int) *genai.GenerateContentConfig {
	if maxOutputTokens <= 0 {
		return nil
	}
	return &genai.GenerateContentConfig{MaxOutputTokens: int32(maxOutputTokens)}
}

// judgeForceCloseInstruction: appended on the round's last allowed turn, or right after the judge
// repeats an identical tool call - forcedVerdictCallback has already stripped every tool (including
// submit_verdict) this turn, so the model must close with the verdict as plain JSON text; runJudgeRound's
// existing parseVerdict fallback picks it up exactly like a local model that skipped the tool call.
const judgeForceCloseInstruction = "\n\nSTOP - you are out of tool budget for this round; no tools, including submit_verdict, are available on this turn. " +
	"Using ONLY what you have already read and verified above, output your verdict now as a single JSON object and nothing else (no code fence, no other text): " +
	`{"score": <0-10 overall fallback>, "criteria": {"<criterion name>": {"reason": "<why>", "score": <0-10>}, ...}, "feedback": "<concise, actionable - empty if it passes>"}` +
	" Score every criterion the rubric named, from what you have already verified."

// forcedVerdictCallback strips all tools and appends judgeForceCloseInstruction on the round's last
// allowed turn, or the turn right after the judge repeats an identical tool call (model stutter that
// would otherwise burn the rest of the budget repeating itself, #853).
func forcedVerdictCallback(maxIters int) llmagent.BeforeModelCallback {
	turn := 0
	return func(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		turn++
		if turn < maxIters && !repeatsLastToolCall(req.Contents) {
			return nil, nil
		}
		req.Tools = nil
		if req.Config != nil {
			req.Config.Tools = nil
		}
		req.Contents = append(req.Contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: judgeForceCloseInstruction}}})
		return nil, nil
	}
}

// repeatsLastToolCall reports whether the two most recent model function calls in the conversation so
// far are the same tool with the same args (Go's json.Marshal sorts map keys, so this compares stably).
func repeatsLastToolCall(contents []*genai.Content) bool {
	var prev, last *genai.FunctionCall
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil {
				prev, last = last, p.FunctionCall
			}
		}
	}
	if last == nil || prev == nil || last.Name != prev.Name {
		return false
	}
	a, aerr := json.Marshal(last.Args)
	b, berr := json.Marshal(prev.Args)
	return aerr == nil && berr == nil && string(a) == string(b)
}

// verdictArgs: submit_verdict schema. Only score is required.
type verdictArgs struct {
	Score    float64                   `json:"score"`
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Feedback string                    `json:"feedback,omitempty"`
	Findings []findingVerdict          `json:"findings,omitempty"`
}

// newSubmitVerdictTool: builds structured-termination tool (mirrors ADK exitlooptool).
func newSubmitVerdictTool(sink *verdict) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        submitVerdictTool,
		Description: "Record your final verdict and end the evaluation. Call this exactly once, after independently verifying the answer against every rubric criterion - and, when the prompt lists staged findings to verify, after recording a result for each one in `findings`.",
	}, func(ctx adkagent.Context, args verdictArgs) (map[string]any, error) {
		v := verdict{Score: args.Score, Criteria: args.Criteria, Feedback: args.Feedback, Findings: args.Findings}
		normalizeScale(&v)
		*sink = v
		ctx.Actions().Escalate = true
		ctx.Actions().SkipSummarization = true
		return map[string]any{"recorded": true}, nil
	})
}

// buildJudgePrompt: assembles judge's user message (constitution, rubric, question, ledger, knownFailures, answer).
func buildJudgePrompt(constitution, rubric, nodeTask string, question *genai.Content, answer, changedFiles string, act workerActivity, knownFailures string) string {
	var sb strings.Builder
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Scoring rubric:\n")
	sb.WriteString(rubric)
	// Score against the node's own task, not the whole background request.
	if strings.TrimSpace(nodeTask) != "" {
		sb.WriteString("\n\nWHAT YOU ARE SCORING - this node's own task, and nothing else:\n")
		sb.WriteString(nodeTask)
		sb.WriteString("\n\nThe request below is BACKGROUND: it is the whole job, most of which belongs to " +
			"OTHER nodes. Do NOT penalise this node for work the task above does not ask of it - a read-only " +
			"research node that committed no code has not failed; that was never its job.")
	}
	sb.WriteString("\n\nUser's question:\n")
	sb.WriteString(contentPlainText(question))
	if ws := buildWorkspaceSection(act); ws != "" {
		sb.WriteString("\n\n")
		sb.WriteString(ws)
	}
	if changedFiles != "" {
		sb.WriteString("\n\n")
		sb.WriteString(changedFiles)
	}
	if knownFailures != "" {
		sb.WriteString("\n\n")
		sb.WriteString(knownFailures)
	}
	sb.WriteString("\n\nAnswer to judge:\n")
	sb.WriteString(answer)
	return sb.String()
}

// judgeKnownFailuresHeader: tells judge not to re-score already-failed criteria.
const judgeKnownFailuresHeader = "The following criteria already FAILED a deterministic, code-owned check before you were asked to judge - they are decided, not yours to score. Do not re-score them and do not let them stand in for the rest of your assessment: judge everything else on its own merits, and make sure your feedback addresses what's left.\n"

// judgeKnownFailuresSection: formats below-threshold entries for the judge prompt.
func judgeKnownFailuresSection(det map[string]criterionScore, threshold float64) string {
	var names []string
	for name, c := range det {
		if c.Score < threshold {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names) // stable order across runs (map iteration is random)
	var sb strings.Builder
	sb.WriteString(judgeKnownFailuresHeader)
	for _, name := range names {
		fmt.Fprintf(&sb, "- %s: %s\n", name, strings.TrimSpace(det[name].Reason))
	}
	return sb.String()
}

// changedFilesBudget/perFileBudget/maxChangedFiles: cap changed-file content in judge prompt.
const (
	changedFilesBudget = 24000
	perFileBudget      = 8000
	maxChangedFiles    = 12
)

// changedFilesCoverage: how many of the worker's changed files the judge
// prompt actually carried, so a verdict over a capped subset doesn't read the
// same as one over the whole change (#779). Zero value means "not applicable"
// (reviewer nodes diff the clone directly, not act.written).
type changedFilesCoverage struct {
	Scored int
	Total  int
	// Capped is true only when maxChangedFiles/changedFilesBudget cut the loop
	// short - NOT when Scored<Total merely because an individual file failed to
	// resolve/read (deleted-after-write etc.). Only the cap is "truncation";
	// an unrelated missing file isn't, and must not raise the note.
	Capped bool
}

// applyChangedFilesCoverage records the coverage on the verdict and, only
// when it actually cut something, appends a note to the feedback - never a
// note when everything fit (#779).
func applyChangedFilesCoverage(v verdict, cov changedFilesCoverage) verdict {
	v.ChangedFilesScored, v.ChangedFilesTotal = cov.Scored, cov.Total
	if !cov.Capped {
		return v
	}
	note := fmt.Sprintf("The judge scored %d of %d changed files - the rest didn't fit the judge's file/size cap and were not reviewed.", cov.Scored, cov.Total)
	if strings.TrimSpace(v.Feedback) == "" {
		v.Feedback = note
	} else {
		v.Feedback = v.Feedback + "\n\n" + note
	}
	return v
}

// changedFilesSection: review nodes use clone diff; implement nodes use full content + diff.
func changedFilesSection(cfg Config, act workerActivity) (string, changedFilesCoverage) {
	if cfg.IsReviewer {
		return reviewVerdictLine(act) + stagedFindingsSection(act) + buildReviewDiffSection(cfg), changedFilesCoverage{}
	}
	written, coverage := buildChangedFilesSection(act, cfg.Workspace, cfg.WorkspaceUserID, cfg.ChatID)
	diff := buildImplementDiffSection(cfg)
	switch {
	case diff == "":
		return written, coverage
	case written == "":
		return diff, coverage
	default:
		return diff + "\n\n" + written, coverage
	}
}

// reviewVerdictLine: surfaces the staged review verdict as a fact for the judge. "" when nothing staged.
func reviewVerdictLine(act workerActivity) string {
	sd, ok := act.stagedDelivery["review"]
	if !ok || sd.Event == "" {
		return ""
	}
	return "Staged review verdict: " + sd.Event + "\n\n"
}

// buildChangedFilesSection: re-reads worker's files through same jail so judge scores real source, not self-report.
// Also reports coverage: how many of act.written actually made it into the section.
func buildChangedFilesSection(act workerActivity, jail *workspace.Jail, userID, chatID string) (string, changedFilesCoverage) {
	if len(act.written) == 0 || jail == nil {
		return "", changedFilesCoverage{}
	}
	var sb strings.Builder
	total := 0
	shown := 0
	capped := false
	for _, rel := range act.written {
		if shown >= maxChangedFiles || total >= changedFilesBudget {
			capped = true
			break
		}
		abs, err := jail.Resolve(userID, chatID, rel)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue // deleted-after-write, moved, or unreadable - skip
		}
		body := boundExcerpt(string(raw), perFileBudget)
		if rem := changedFilesBudget - total; len(body) > rem {
			body = boundExcerpt(body, rem)
		}
		if shown == 0 {
			sb.WriteString("ACTUAL CURRENT CONTENT OF THE FILES THE WORKER CREATED/CHANGED (read these; do not trust the answer's description of them - judge the code that is really on disk: does the deliverable actually build, is it complete, does it match the repo's conventions, are the tests real):\n")
		}
		fmt.Fprintf(&sb, "\n----- %s -----\n", rel)
		sb.WriteString(body)
		sb.WriteString("\n")
		total += len(body)
		shown++
	}
	return sb.String(), changedFilesCoverage{Scored: shown, Total: len(act.written), Capped: capped}
}

const judgeCharsPerToken = 4 // bytes/4, same as compaction estimator

const judgeOutputReserveTokens = 2_000 // reserved for judge's reply

// defaultJudgeContextWindow: fallback when Config.JudgeContextWindow is unset (0).
const defaultJudgeContextWindow = 32_768

// minJudgeAnswerChars: floor for answer size even when fixed sections blow the budget.
const minJudgeAnswerChars = 2_000

// judgeCharBudget: derives judge-prompt byte budget from context window minus output reserve.
func judgeCharBudget(cfg Config) int {
	window := cfg.JudgeContextWindow
	if window <= 0 {
		window = defaultJudgeContextWindow
	}
	tokens := window - judgeOutputReserveTokens
	if tokens <= 0 {
		tokens = window
	}
	return tokens * judgeCharsPerToken
}

// fitJudgeAnswer: clamps answer so judge prompt fits budget. shrinkFactor < 1.0 = harder clamp for retry.
func fitJudgeAnswer(cfg Config, question *genai.Content, answer, changedFiles, knownFailures string, act workerActivity, shrinkFactor float64) string {
	budget := judgeCharBudget(cfg)
	full := buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, question, answer, changedFiles, act, knownFailures)
	over := len(full) - budget
	if over <= 0 && shrinkFactor >= 1.0 {
		return answer
	}
	fixed := len(full) - len(answer) // everything the judge prompt carries besides the answer
	target := budget - fixed
	if shrinkFactor < 1.0 {
		target = int(float64(len(answer)) * shrinkFactor)
	}
	if target < minJudgeAnswerChars {
		target = minJudgeAnswerChars
	}
	if target >= len(answer) {
		return answer
	}
	return boundExcerpt(answer, target)
}

// judgeRetryAttempts/judgeRetryBaseDelay: backoff for transient model-endpoint faults.
const judgeRetryAttempts = 3
const judgeRetryBaseDelay = 400 * time.Millisecond

// isTransientJudgeErr: endpoint fault worth retrying (not bad request/auth).
func isTransientJudgeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"status 502", "status 503", "status 504", "status 429", "timeout", "deadline exceeded", "connection reset", "connection refused", "eof"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// runJudgeAgent: budgets judge prompt, retries transient faults, falls back to one harder-clamped retry.
// Named returns so the deferred coverage stamp is a single choke point across every exit (#779) instead of
// duplicated at each return.
func runJudgeAgent(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer string, act workerActivity, det map[string]criterionScore, emit func(*genai.Part) bool) (v verdict, err error) {
	changedFiles, coverage := changedFilesSection(cfg, act)
	known := judgeKnownFailuresSection(det, cfg.Threshold)
	fitted := fitJudgeAnswer(cfg, question, answer, changedFiles, known, act, 1.0)
	defer func() {
		if err == nil {
			v = applyChangedFilesCoverage(v, coverage)
		}
	}()

	var readc *readCounter
	for attempt := 1; attempt <= judgeRetryAttempts; attempt++ {
		v, readc, err = runJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, act, emit)
		if err == nil || ctx.Err() != nil || !isTransientJudgeErr(err) || attempt == judgeRetryAttempts {
			break
		}
		delay := judgeRetryBaseDelay * time.Duration(1<<uint(attempt-1))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			v, err = verdict{}, ctx.Err()
			return
		}
	}

	// A round that ran but never reached a verdict (model stutter exhausting the
	// budget, #853) gets exactly one retry with a fresh session before surfacing
	// unvetted - shrinking the answer (the fallback below) wouldn't fix a stutter,
	// so this returns unconditionally rather than falling into that path.
	if errors.Is(err, ErrJudgeNoVerdict) && ctx.Err() == nil {
		slog.Warn("judge ended without a verdict; retrying the round once with a fresh session",
			"component", "vetting", "agent", cfg.Agent)
		v, readc, err = runJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, act, emit)
		if err == nil {
			v = finishJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, act, emit, v, readc)
		}
		return
	}

	if err == nil || ctx.Err() != nil {
		v = finishJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, act, emit, v, readc)
		return
	}
	retryAnswer := fitJudgeAnswer(cfg, question, fitted, changedFiles, known, act, 0.5)
	if retryAnswer == fitted {
		v = verdict{} // nothing left to shrink; the retry would repeat the same call
		return
	}
	v, _, err = runJudgeRound(ctx, factory, cfg, question, retryAnswer, changedFiles, known, act, emit)
	return
}

// finishJudgeRound: a PASS verdict backed by zero judge reads is discarded and re-judged once before
// being trusted (second offence accepted - one wasted round is the ceiling). No-op otherwise.
func finishJudgeRound(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, fitted, changedFiles, known string, act workerActivity, emit func(*genai.Part) bool, v verdict, readc *readCounter) verdict {
	if !unreadPass(readc, v) {
		return v
	}
	slog.Warn("judge passed without reading the repo; re-judging once",
		"component", "vetting", "agent", cfg.Agent, "score", v.Score)
	v2, readc2, err2 := runJudgeRound(ctx, factory, cfg, question,
		fitted+"\n\n"+unreadPassFeedback, changedFiles, known, act, emit)
	if err2 != nil {
		slog.Warn("re-judge failed; keeping the unread verdict", "component", "vetting", "err", err2)
		return v
	}
	if unreadPass(readc2, v2) {
		slog.Warn("judge passed without reading the repo again; accepting the verdict",
			"component", "vetting", "agent", cfg.Agent, "score", v2.Score)
	}
	return v2
}

// judgeRepeat* tune the runaway-generation guard shared by the judge, skeptic,
// and plan judge: a degenerate loop stutters far faster than any legitimate
// verdict grows, so watching a bounded trailing window is enough (#889: 18K+
// tokens looped on one verdict before this existed).
const (
	judgeRepeatWindowChars  = 9000 // trailing text rescanned per check
	judgeRepeatMinUnitChars = 8    // shortest repeat unit checked (a short stutter phrase)
	judgeRepeatMaxUnitChars = 150  // longest repeat unit checked (a couple of sentences)
	judgeRepeatTripChars    = 8000 // contiguous repeat span that aborts the round (~2K tokens at judgeCharsPerToken)
	judgeRepeatCheckStride  = 200  // re-scan only every this many new chars (the scan itself is O(window×units))
)

// repeatLoopDetector watches a model's streamed text for a runaway repeat
// loop. Not safe for concurrent use; one instance per round.
type repeatLoopDetector struct {
	buf        strings.Builder
	sinceCheck int
	tripped    bool
}

// observe appends newly streamed text and, once enough has accumulated since
// the last scan, checks the trailing window for a repeat loop.
func (d *repeatLoopDetector) observe(s string) {
	if d.tripped || s == "" {
		return
	}
	d.buf.WriteString(s)
	d.sinceCheck += len(s)
	if d.sinceCheck < judgeRepeatCheckStride {
		return
	}
	d.sinceCheck = 0
	tail := d.buf.String()
	if len(tail) > judgeRepeatWindowChars {
		tail = tail[len(tail)-judgeRepeatWindowChars:]
	}
	if repeatingTailSpan(tail, judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars) >= judgeRepeatTripChars {
		d.tripped = true
	}
}

// repeatingTailSpan returns the length of the longest contiguous span at the
// end of s made of the same repeated unit (0 if none found), checking unit
// sizes in [minUnit, maxUnit]. Phase-aligned to the end of s rather than to
// fixed byte offsets, since a loop's start position is never known in advance.
func repeatingTailSpan(s string, minUnit, maxUnit int) int {
	n := len(s)
	best := 0
	for unit := minUnit; unit <= maxUnit && unit*2 <= n; unit++ {
		pattern := s[n-unit:]
		span := unit
		for pos := n - unit; pos-unit >= 0 && s[pos-unit:pos] == pattern; pos -= unit {
			span += unit
		}
		if span > unit && span > best { // a single occurrence isn't a repeat
			best = span
		}
	}
	return best
}

// runJudgeRound: isolated agentic judge round (own runner + in-memory session). Falls back to text parsing.
func runJudgeRound(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer, changedFiles, knownFailures string, act workerActivity, emit func(*genai.Part) bool) (verdict, *readCounter, error) {
	maxIters := cfg.JudgeMaxIterations
	if maxIters <= 0 {
		maxIters = defaultJudgeMaxIterations
	}

	var sink verdict
	judgeAgent, reads, err := factory(&sink, maxIters, cfg.JudgeMaxOutputTokens)
	if err != nil {
		return verdict{}, nil, fmt.Errorf("vetting: build judge agent: %w", err)
	}
	jr, err := runner.New(runner.Config{
		AppName:           "quack-judge",
		Agent:             judgeAgent,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return verdict{}, nil, fmt.Errorf("vetting: judge runner: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	promptText := buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, question, answer, changedFiles, act, knownFailures)
	// Stamp advisor-thread token so judge resolves fs tools into the worker's node scope.
	if cfg.AdvisorToken != "" {
		promptText += "\n\n" + AdvisorThreadMarker(cfg.AdvisorToken)
	}
	parts := []*genai.Part{{Text: promptText}}
	for _, p := range question.Parts {
		if p != nil && p.InlineData != nil {
			parts = append(parts, p)
		}
	}
	content := &genai.Content{Role: "user", Parts: parts}

	var (
		submitted bool
		turns     int
		accum     strings.Builder
		repeats   repeatLoopDetector
	)
	for ev, err := range jr.Run(runCtx, "judge", "verdict", content, adkagent.RunConfig{}) {
		if err != nil {
			return verdict{}, reads, err
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
				// suppress from generic tool-call activity; success is confirmed
				// on the matching FunctionResponse below - a schema-rejected or
				// garbled call (e.g. truncated by the output cap) must not be
				// mistaken for a submitted verdict (#889).
			case p.FunctionResponse != nil && p.FunctionResponse.Name == submitVerdictTool:
				if _, failed := p.FunctionResponse.Response["error"]; !failed {
					submitted = true // handler ran; sink is populated
				}
			case p.Thought && p.Text != "":
				repeats.observe(p.Text)
				if !emit(stream.ThinkingPart(p.Text)) {
					return verdict{}, reads, context.Canceled
				}
			case p.FunctionCall != nil:
				if !emit(&genai.Part{FunctionCall: p.FunctionCall}) {
					return verdict{}, reads, context.Canceled
				}
			case p.FunctionResponse != nil:
				if !emit(&genai.Part{FunctionResponse: p.FunctionResponse}) {
					return verdict{}, reads, context.Canceled
				}
			case p.Text != "":
				// Local model emits reasoning as plain text, not Thought parts.
				accum.WriteString(p.Text)
				repeats.observe(p.Text)
				if !emit(stream.ThinkingPart(p.Text)) {
					return verdict{}, reads, context.Canceled
				}
			}
		}
		if ev.TurnComplete {
			turns++
		}
		// Safety cap: prevent infinite loop if judge never calls submit_verdict,
		// or a runaway repeat loop is decoding the same text forever (#889).
		if turns > maxIters || repeats.tripped {
			if repeats.tripped {
				slog.Warn("judge round aborted: runaway repeat detected mid-generation",
					"component", "vetting", "agent", cfg.Agent)
			}
			cancel()
			break
		}
	}

	if submitted {
		return aggregateVerdict(sink), reads, nil
	}
	// Fallback: judge ended without a structured verdict. Try its text, else fail.
	if v, perr := parseVerdict(accum.String()); perr == nil {
		return v, reads, nil
	}
	return verdict{}, reads, ErrJudgeNoVerdict
}

// ErrJudgeNoVerdict: the judge model ran - read files, spent its iteration
// budget - and never called submit_verdict. Distinct from a transport/model
// failure (the judge was never reachable at all) so the caller can tell the
// reader which one happened instead of calling both "unavailable" (#779).
var ErrJudgeNoVerdict = errors.New("vetting: judge ended without a verdict")

// markdownLinkRe extracts inline Markdown link targets - web URLs AND local
// paths ([games-repo/app/games.ts](games-repo/app/games.ts)): repo-exploration
// and coding nodes cite the files they read, not web pages.
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// codeCiteRe matches the code-explorer's inline file-read citation format -
// "<repo>@<path>" or "<repo>@<path>:<line-range>" (e.g.
// "quack@internal/dag/executor.go" or "quack@server/core/worker.go:207-216")
// - used instead of Markdown links when citing files read off a clone. The
// path segment requires at least one "/" so this can't mistake an email
// address (a@b.com) for a citation.
var codeCiteRe = regexp.MustCompile(`\b([\w.-]+)@([\w.-]+(?:/[\w.-]+)+)(?::(\d+(?:-\d+)?))?`)

// citationScore: deterministic grade per cited link. Web URLs against session ledger; local code citations against clone on disk.
// Web layers: fetched=1.00, under cloned repo=1.00, searched=0.75, same host fetched=0.50, same host searched=0.25, neither=0.00.
// Worker-facing meaning of these tiers lives in citeReasonLegend below - keep the two in sync.
func citationScore(answer string, act workerActivity, cloneRoots []string) (score float64, details []citationDetail, ok bool) {
	if len(act.fetched) == 0 && len(act.seen) == 0 && len(act.clonedRepos) == 0 && len(cloneRoots) == 0 {
		return 0, nil, false
	}
	fetchedURL, fetchedHost := normalizedSets(slices.Collect(maps.Keys(act.fetched)))
	seenURL, seenHost := normalizedSets(slices.Collect(maps.Keys(act.seen)))
	cloneNorms := normalizedCloneURLs(act.clonedRepos)

	dedup := make(map[string]struct{})
	var sum float64
	for _, m := range markdownLinkRe.FindAllStringSubmatch(answer, -1) {
		if len(m) < 2 {
			continue
		}
		target := strings.TrimSpace(m[1])
		u, err := url.Parse(target)
		if err != nil || target == "" {
			continue
		}

		var key string
		var s float64
		switch {
		case u.Scheme == "http" || u.Scheme == "https":
			norm, host := normalizeURL(target)
			if norm == "" {
				continue
			}
			key = norm
			switch {
			case fetchedURL[norm]:
				s = 1.00
			case underClonedRepo(norm, cloneNorms):
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
		case u.Scheme != "":
			continue // mailto:, ftp:, … - not a gradeable citation
		default:
			// Local code citation. u.Path drops a fragment ([x](file.md#L4))
			// and query. Disk-verified, not ledger-checked - see the doc comment.
			np := normalizePath(u.Path)
			if np == "" {
				continue // pure in-document anchor like (#section)
			}
			key = np
			s = diskCiteScore(cloneRoots, []string{np}, "")
		}
		if _, dup := dedup[key]; dup {
			continue
		}
		dedup[key] = struct{}{}
		details = append(details, citationDetail{url: target, score: s})
		sum += s
	}
	for _, m := range codeCiteRe.FindAllStringSubmatch(answer, -1) {
		repo, path, lineRange := m[1], normalizePath(m[2]), m[3]
		if path == "" {
			continue
		}
		key := "code:" + repo + "@" + path
		if _, dup := dedup[key]; dup {
			continue
		}
		dedup[key] = struct{}{}
		// A code citation is repo-relative, but the clone root may correspond
		// to it either bare (repo-relative) or clone-dir-prefixed (repo/path)
		// - try both.
		s := diskCiteScore(cloneRoots, []string{path, repo + "/" + path}, lineRange)
		details = append(details, citationDetail{url: m[0], score: s})
		sum += s
	}
	if len(details) == 0 {
		return 0, nil, false
	}
	return sum / float64(len(details)), details, true
}

// maxCiteLineScanBytes: bounds cited-file reads for line-range confirmation.
const maxCiteLineScanBytes = 512 * 1024

// diskCiteScore: verifies code citation against clone on disk. Tries candidate with and without root base name.
func diskCiteScore(cloneRoots []string, candidates []string, lineRange string) float64 {
	endLine := 0
	if lineRange != "" {
		end := lineRange
		if _, after, found := strings.Cut(lineRange, "-"); found {
			end = after
		}
		n, err := strconv.Atoi(end)
		if err != nil {
			return 0
		}
		endLine = n
	}
	for _, root := range cloneRoots {
		base := filepath.Base(root)
		for _, cand := range candidates {
			tries := []string{cand}
			if stripped, ok := strings.CutPrefix(cand, base+"/"); ok {
				tries = append(tries, stripped)
			}
			for _, t := range tries {
				abs, ok := resolveUnderRoot(root, t)
				if !ok {
					continue
				}
				info, err := os.Stat(abs)
				if err != nil || info.IsDir() {
					continue
				}
				if endLine == 0 || fileHasLine(abs, endLine) {
					return 1.0
				}
			}
		}
	}
	return 0
}

// resolveUnderRoot: joins rel under root, rejecting path traversal.
func resolveUnderRoot(root, rel string) (string, bool) {
	if root == "" || rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	root = filepath.Clean(root)
	joined := filepath.Join(root, rel)
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

// fileHasLine: does path have at least n lines? Scans at most maxCiteLineScanBytes.
func fileHasLine(path string, n int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxCiteLineScanBytes))
	count := 0
	for sc.Scan() {
		count++
		if count >= n {
			return true
		}
	}
	return false
}

// normalizedCloneURLs: normalizes clone URLs (normalizeURL + trim .git suffix).
func normalizedCloneURLs(clones []string) []string {
	out := make([]string, 0, len(clones))
	for _, c := range clones {
		n, _ := normalizeURL(c)
		if n = strings.TrimSuffix(n, ".git"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// underClonedRepo: does normalized URL point at/under a cloned repo? Segment-aware.
func underClonedRepo(norm string, cloneNorms []string) bool {
	n := strings.TrimSuffix(norm, ".git")
	for _, c := range cloneNorms {
		if n == c || strings.HasPrefix(n, c+"/") {
			return true
		}
	}
	return false
}

// normalizePath: trims "./" prefix and trailing "/". No symlink/.. resolution.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return strings.TrimRight(p, "/")
}

// citationDetail: one cited link's deterministic backing score.
type citationDetail struct {
	url   string
	score float64
}

// maxCiteReasonLinks bounds unbacked links named in a cites_sources reason -
// a forty-link answer must not produce forty lines of feedback (#789).
const maxCiteReasonLinks = 10

// citeReasonLegend: what each citationScore tier (~line 563) means and the
// remedy, for a revising worker with no other context.
const citeReasonLegend = "backing tiers: 1.0=fetched/on-clone (trust it), 0.75=seen in search results only (fetch to confirm before keeping), 0.5/0.25=same host but exact page never seen (likely invented - re-find or drop), 0=never seen anywhere (fabricated - drop it). Score is the mean across all cited links, so one weak link among many matters less than an isolated one."

// citeReason names the links that scored below full backing, worst first, so
// the worker fixes the most-damning ones first if the list gets capped.
func citeReason(score float64, details []citationDetail) string {
	reason := fmt.Sprintf("deterministic: %d cited link(s), mean backing %.2f", len(details), score)
	var unbacked []citationDetail
	for _, d := range details {
		if d.score < 1.0 {
			unbacked = append(unbacked, d)
		}
	}
	if len(unbacked) == 0 {
		return reason
	}
	// Worst-first (stable) so the cap below elides the least-bad links, not the most-damning ones.
	sort.SliceStable(unbacked, func(i, j int) bool { return unbacked[i].score < unbacked[j].score })
	elided := 0
	if len(unbacked) > maxCiteReasonLinks {
		elided = len(unbacked) - maxCiteReasonLinks
		unbacked = unbacked[:maxCiteReasonLinks]
	}
	parts := make([]string, len(unbacked))
	for i, d := range unbacked {
		parts[i] = fmt.Sprintf("%s (%.2f)", d.url, d.score)
	}
	reason += " - not fully backed: " + strings.Join(parts, ", ")
	if elided > 0 {
		reason += fmt.Sprintf(" (and %d more elided)", elided)
	}
	reason += ". " + citeReasonLegend
	return reason
}

// normalizeURL: lowercases scheme+host, drops fragment, trims trailing slash.
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

// normalizedSets: returns normalized URL and host sets.
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

// minAnswerChars: lengthScore's only threshold - anything non-empty passes.
const minAnswerChars = 1

// lengthScore: 0.0 for empty answer, 1.0 otherwise. Deliberately minimal to avoid penalizing concise answers.
func lengthScore(answer string) float64 {
	if strings.TrimSpace(answer) == "" {
		return 0.0
	}
	return 1.0
}

// buildActivitySection: summarises worker's retrieval for the prompt. Omitted when empty.
func buildActivitySection(act workerActivity) string {
	if len(act.searches) == 0 && len(act.fetched) == 0 && len(act.workspace) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(act.searches) > 0 || len(act.fetched) > 0 {
		sb.WriteString("Session activity (retrieval the worker performed - do not contradict this):\n")
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
	}
	// Workspace ledger: reminds worker what it has (and has not) done.
	if ws := buildWorkspaceSection(act); ws != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(ws)
	}
	return sb.String()
}

// Caps on the revise/finalize prompt sections (contents[0] - context compaction cannot touch).
const (
	maxOriginalQuestionChars = 24_000
	maxPreviousAnswerChars   = 16_000
	maxActivitySectionChars  = 32_000
	maxFeedbackChars         = 16_000
)

// boundExcerpt: head+tail excerpt with truncation marker. Favours head at 60/40.
func boundExcerpt(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	const marker = "\n\n…[excerpt truncated to fit the context window - the full material is in your own session/tools; re-read the files if you need more detail]…\n\n"
	keep := maxChars - len(marker)
	if keep <= 0 {
		return strings.ToValidUTF8(s[:maxChars], "")
	}
	head := keep * 3 / 5
	return strings.ToValidUTF8(s[:head], "") + marker + strings.ToValidUTF8(s[len(s)-(keep-head):], "")
}

// buildRevisionContent: re-invokes worker to address judge feedback. Every section bounded (boundExcerpt).
func buildRevisionContent(constitution string, question *genai.Content, answer, feedback string, act workerActivity, citationOnly bool) *genai.Content {
	var sb strings.Builder
	if citationOnly {
		// The answer's substance passed; only cites_sources failed. This is a
		// formatting pass, not re-research: the worker already fetched the URLs
		// (listed in the activity section below), so re-fetching them wastes
		// tokens and time. Tell it to attach what it has.
		sb.WriteString("Your previous answer is substantively fine - the ONLY problem is missing inline citations. " +
			"You already retrieved the sources listed below (URLs you fetched and searched); attach them inline as Markdown links to the claims they support. " +
			"Do NOT re-fetch or search again - this is purely a citation-formatting fix. " +
			"Then output only the corrected answer with no preamble or commentary.\n\n")
	} else {
		sb.WriteString("An independent reviewer evaluated your previous answer and it must be improved before it can be returned. " +
			"Address the reviewer's feedback below: use your tools to fix the gaps - re-fetch and verify sources, correct or remove unsupported claims, add missing citations. " +
			"If you're unsure how to address this feedback, consult your advisor (ask_advisor) before revising - it knows this task's goal and can help you tell what actually needs to change. " +
			"Then output only the corrected answer with no preamble or commentary.\n\n")
	}
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Reviewer feedback to address:\n")
	sb.WriteString(boundExcerpt(feedback, maxFeedbackChars))
	sb.WriteString("\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n")
	}
	sb.WriteString("Original question:\n")
	sb.WriteString(boundExcerpt(contentPlainText(question), maxOriginalQuestionChars))
	sb.WriteString("\n\nYour previous answer:\n")
	sb.WriteString(boundExcerpt(answer, maxPreviousAnswerChars))
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// buildFinalizeContent: asks worker to write final answer when round 0 ended without one.
func buildFinalizeContent(question *genai.Content, act workerActivity) *genai.Content {
	var sb strings.Builder
	sb.WriteString("A response of 0 length was received. If you have finished your research, " +
		"do not call any more tools - just write your complete response again now, using everything " +
		"you found above. If you are not done with your research, there was likely an error: please " +
		"make sure you close all of your reasoning/thinking and tool-call blocks and continue " +
		"your research. Output only the answer with no preamble or commentary.\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n")
	}
	sb.WriteString("Question:\n")
	sb.WriteString(boundExcerpt(contentPlainText(question), maxOriginalQuestionChars))
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// continuationMarker: signals that this turn continues an unfinished task.
const continuationMarker = "CONTINUE THE TASK - it is not finished."

// buildContinuationPrompt: tool-bearing continuation directive (do the remaining work, not a write-up).
func buildContinuationPrompt(task string, act workerActivity, checks []string, readOnly, hasDeliverTarget, isReviewer, existingPR bool) string {
	var sb strings.Builder
	sb.WriteString(continuationMarker + "\n\n" +
		"Your last turn produced no answer, or produced an answer for work you have not actually delivered. " +
		"You are MID-TASK, not done. This is not a request for a summary.\n\n" +
		"Continue now, using your tools:\n" +
		"- DO the remaining work rather than describing it. Writing a file's contents into your answer is NOT writing the file - call write_file/edit_file.\n" +
		"- Run the checks the task requires and fix whatever they surface.\n" +
		"- When the task calls for it, commit your work, push the branch, and open the pull request.\n" +
		"- When the task calls for a posted review, record your findings with github_add_review_comment and submit them with github_submit_review. Writing findings into your answer is NOT posting a review.\n" +
		"- When the task calls for a review of a code change, EXECUTE the change with run_command before you judge it - run its tests, and write a throwaway harness that drives the code and prints what it does. Reading is not verification.\n" +
		"- Only once the work is actually done, report what you DID - past tense, evidenced by the tool calls you made.\n\n")
	if len(checks) > 0 {
		sb.WriteString("Checks this node must pass: " + strings.Join(checks, ", ") + "\n\n")
	}
	var gaps []string
	for _, c := range incompleteCriteria(task, act, readOnly, hasDeliverTarget, isReviewer, existingPR) {
		if c.Score < 1 {
			gaps = append(gaps, "Known gap: "+c.Reason+"\n\n")
		}
	}
	sort.Strings(gaps) // stable order across runs (map iteration is random)
	sb.WriteString(strings.Join(gaps, ""))
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n")
	}
	sb.WriteString("Original task:\n")
	sb.WriteString(boundExcerpt(task, maxOriginalQuestionChars))
	return sb.String()
}

// normalizeScale: converts 0-10 rubric scale to internal 0-1 axis. Idempotent.
func normalizeScale(v *verdict) {
	maxScore := v.Score
	for _, c := range v.Criteria {
		if c.Score > maxScore {
			maxScore = c.Score
		}
	}
	if maxScore <= 1.0 {
		return // already on the 0–1 axis
	}
	v.Score /= 10
	for name, c := range v.Criteria {
		c.Score /= 10
		v.Criteria[name] = c
	}
}

// aggregateVerdict: weakest-link gating - lowest criterion is the overall score. Clamped [0,1].
func aggregateVerdict(v verdict) verdict {
	applyFindingsVerdict(&v)
	// Weakest-link gating: lowest criterion is the score.
	if len(v.Criteria) > 0 {
		lowest := 1.0
		for _, c := range v.Criteria {
			if c.Score < lowest {
				lowest = c.Score
			}
		}
		v.Score = lowest
	}
	if v.Score < 0 {
		v.Score = 0
	}
	if v.Score > 1 {
		v.Score = 1
	}
	return v
}

// emitEvaluationResults: records gen_ai.evaluation.result event per criterion. runID stands in for responseID.
func emitEvaluationResults(ctx context.Context, responseID string, v verdict) {
	names := make([]string, 0, len(v.Criteria))
	for name := range v.Criteria {
		names = append(names, name)
	}
	sort.Strings(names) // map iteration is random; a stable emit order matters for replay diffing
	for _, name := range names {
		cs := v.Criteria[name]
		otelobs.EmitLog(ctx, judgeScope, "",
			otellog.String(otelobs.GenAIResponseID, responseID),
			otellog.String(otelobs.GenAIEvaluationName, name),
			otellog.Float64(otelobs.GenAIEvaluationScore, cs.Score),
			otellog.String(otelobs.GenAIEvaluationExplain, cs.Reason),
		)
	}
}

// parseVerdict: fallback JSON parser (tolerates ```json fence, truncated JSON, misplaced fields).
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
	// object and ignores any trailing content - including a duplicated blob.
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

	normalizeScale(&v)
	return aggregateVerdict(v), nil
}

// runWriterFresh: recovers empty worker draft via tool-less writer in a fresh runner (re-invoking worker loses finalize prompt).
func runWriterFresh(ctx context.Context, m model.LLM, content *genai.Content) (string, error) {
	if m == nil {
		return "", fmt.Errorf("vetting: no writer model for empty-answer recovery")
	}
	writer, err := llmagent.New(llmagent.Config{
		Name:        "finalize-writer",
		Description: "Composes the final answer from the findings provided, without tools.",
		Model:       m,
		Instruction: "You are a writer. Using ONLY the findings and instructions in the message, write the complete final answer now. You have no tools - do not attempt to call any; compose directly from what you are given.",
	})
	if err != nil {
		return "", fmt.Errorf("vetting: build writer: %w", err)
	}
	wr, err := runner.New(runner.Config{AppName: "quack-writer", Agent: writer, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		return "", fmt.Errorf("vetting: writer runner: %w", err)
	}
	var out strings.Builder
	for ev, rerr := range wr.Run(ctx, "writer", "finalize", content, adkagent.RunConfig{}) {
		if rerr != nil {
			return "", rerr
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	return stream.StripThinking(out.String()), nil
}
