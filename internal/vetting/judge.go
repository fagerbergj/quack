package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.opentelemetry.io/otel/attribute"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
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
)

// judgeBlocks are system/judge's clause blocks, every one required: a stored
// version missing one would render a judge told less than it actually holds.
var judgeBlocks = []string{"head", "no_tools", "read_tools", "artifact_tools", "skills", "tail"}

// judgePrompt is system/judge as resolved for ONE judge round, rendered up
// front so the version the round's ledger coords record is the one it ran on.
type judgePrompt struct {
	art    artifactsrc.Artifact
	blocks map[string]string
}

// judgeTemplates caches the parsed system/judge per version across rounds.
var judgeTemplates artifactsrc.TemplateCache

// resolveJudgePrompt renders every clause block. A stored version that will not
// parse, or is missing a block, falls back to the shipped file (artifactsrc.Render)
// instead of erroring - a bad prompt edit must not disable the gate fleet-wide.
func resolveJudgePrompt(ctx context.Context, res *artifactsrc.Resolver) (judgePrompt, error) {
	blocks := map[string]string{}
	art, err := artifactsrc.Render(ctx, res, &judgeTemplates, "system/judge", func(t *template.Template) error {
		clear(blocks)
		for _, b := range judgeBlocks {
			var sb strings.Builder
			if err := t.ExecuteTemplate(&sb, b, nil); err != nil {
				return fmt.Errorf("block %q: %w", b, err)
			}
			if blocks[b] = strings.TrimSpace(sb.String()); blocks[b] == "" {
				return fmt.Errorf("block %q is empty", b)
			}
		}
		return nil
	})
	if err != nil {
		return judgePrompt{}, fmt.Errorf("vetting: system/judge: %w", err)
	}
	return judgePrompt{art: art, blocks: blocks}, nil
}

// judgePromptFor is the round's system/judge: prepareJudge normally resolved it
// already, so the round's ledger coords carry the same version; a caller that
// builds no coords (the plan judge, tests) resolves its own here.
func judgePromptFor(ctx context.Context, cfg Config) (judgePrompt, error) {
	if cfg.judgePrompt.blocks != nil {
		return cfg.judgePrompt, nil
	}
	return resolveJudgePrompt(ctx, cfg.Prompts)
}

// behaviour picks the clauses this judge's actual tools earn it. artifact_tools
// is unconditional: every round carries list_artifacts/read_artifact (#1497).
func (p judgePrompt) behaviour(hasReadTools, hasSkills bool) string {
	parts := []string{p.blocks["head"]}
	if hasReadTools {
		parts = append(parts, p.blocks["read_tools"])
	} else {
		parts = append(parts, p.blocks["no_tools"])
	}
	parts = append(parts, p.blocks["artifact_tools"])
	if hasSkills {
		parts = append(parts, p.blocks["skills"])
	}
	return strings.Join(append(parts, p.blocks["tail"]), " ")
}

// criterionScore: per-criterion assessment, normalised 0.0-1.0.
type criterionScore struct {
	// Reason: deprecated per #941 - kept accepted on the way in for one release
	// so a judge that ignores the schema change still round-trips; aggregateVerdict
	// copies it into Shortfall when Shortfall is empty.
	Reason    string      `json:"reason,omitempty"`
	Shortfall string      `json:"shortfall,omitempty"` // diagnosis - what fell short
	Fix       string      `json:"fix,omitempty"`       // remedy
	Anchor    *anchorSpec `json:"anchor,omitempty"`    // where in the answer, if locatable
	Score     float64     `json:"score"`
	// Deterministic marks a code-owned criterion (set by mergeDeterministic from
	// computeDeterministicCriteria's provenance, never by the judge) - json:"-" so
	// it never appears in the judge's tool schema or gets round-tripped from its output.
	Deterministic bool `json:"-"`
	// Unscored: the judge's entry had no score key, so Score's 0 is not a verdict.
	Unscored bool `json:"-"`
	// Definition/Scale/Bands/Evidence: envelope metadata, never set by the judge
	// (json:"-") - populated by mergeDeterministic or applyRubricSpecs.
	Definition string         `json:"-"`
	Scale      *scaleSpec     `json:"-"`
	Bands      []bandSpec     `json:"-"`
	Evidence   []evidenceItem `json:"-"`
}

// verdict: structured round score. Score is lowest criterion (weakest-link).
type verdict struct {
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Score    float64                   `json:"score"`
	Passed   bool                      `json:"passed"`
	Feedback string                    `json:"feedback"`
	Findings []findingVerdict          `json:"findings,omitempty"` // per-finding verification; "contradicted" folds into findingsGroundingCriterion
	Memories []memoryVerdict           `json:"memories,omitempty"` // per-recalled-memory vote (#1255 P1); applied only when the round passes

	// ChangedFiles* are set from changedFilesCoverage after the round, not by
	// the model - how much of the diff the judge actually saw (#779).
	ChangedFilesScored int `json:"changed_files_scored,omitempty"`
	ChangedFilesTotal  int `json:"changed_files_total,omitempty"`
}

// JudgeFactory: builds a fresh agentic judge per round, per-factory read-only tools, per-round judgeReadCounters. maxIters wires forcedVerdictCallback so the round's last allowed turn (or a repeated identical tool
// call) forces a text-only verdict instead of silently exhausting the budget (#853). maxOutputTokens caps the round's own reply tokens against a runaway generation loop; <= 0 leaves it uncapped (#889). forced is set true by forcedVerdictCallback the moment it strips tools for a forced close - the
// caller's own signal that this round already spent its last allowed turn (#1235). receivedIDs (#1259): the round's recalled-memory ids, so the tool description and force-close instruction can require votes on the exact set delivered this round, not a generic reminder. artifactTools (#1497): this round's list_artifacts/read_artifact, from cfg.JudgeArtifactTools - per-round, never baked into the factory like readTools.
type JudgeFactory func(prompt judgePrompt, sink *verdict, forced *bool, maxIters, maxOutputTokens int, thinkingLevel string, receivedIDs []string, artifactTools []tool.Tool) (adkagent.Agent, judgeReadCounters, error)

// NewJudgeFactory: builds agentic judge with judgeModel, read-only tools, skillsets, and submit_verdict.
func NewJudgeFactory(judgeModel model.LLM, readTools []tool.Tool, skillsets []tool.Toolset) JudgeFactory {
	hasReadTools, hasSkills := len(readTools) > 0, len(skillsets) > 0
	return func(prompt judgePrompt, sink *verdict, forced *bool, maxIters, maxOutputTokens int, thinkingLevel string, receivedIDs []string, artifactTools []tool.Tool) (adkagent.Agent, judgeReadCounters, error) {
		behaviour, version := prompt.behaviour(hasReadTools, hasSkills), prompt.art.VersionID
		submit, err := newSubmitVerdictTool(sink, receivedIDs)
		if err != nil {
			return nil, judgeReadCounters{}, err
		}
		countedRepo, repoReads := countReads(readTools)
		countedArtifacts, artifactReads := countReads(artifactTools)
		judgeTools := make([]tool.Tool, 0, len(readTools)+len(artifactTools)+1)
		judgeTools = append(judgeTools, countedRepo...)
		judgeTools = append(judgeTools, countedArtifacts...)
		judgeTools = append(judgeTools, submit)
		// judgeTools/behaviour are fixed for this round; only today() moves,
		// so cache instead of rebuilding the prompt on every model call in
		// the round's multi-turn agentic loop.
		assembled := promptbuilder.CacheByDay(
			func(context.Context) string { return version },
			func(context.Context) string { return promptbuilder.Judge(judgeTools, behaviour) })
		a, err := llmagent.New(llmagent.Config{
			Name:        "judge",
			Description: "independent adversarial verifier",
			Model:       judgeModel,
			InstructionProvider: func(rc adkagent.ReadonlyContext) (string, error) {
				return assembled(rc), nil
			},
			Tools:                 judgeTools,
			Toolsets:              skillsets,
			GenerateContentConfig: judgeGenConfig(maxOutputTokens, thinkingLevel),
			BeforeModelCallbacks:  []llmagent.BeforeModelCallback{forcedVerdictCallback(maxIters, forced, receivedIDs)},
		})
		return a, judgeReadCounters{repo: repoReads, artifact: artifactReads}, err
	}
}

// judgeGenConfig caps a judge/plan-judge round's own reply tokens - a
// verdict is a few hundred tokens of JSON, but an ungoverned round can decode
// tens of thousands looping (#889). <= 0 leaves the request uncapped. thinkingLevel is opt-in via gates.judge.thinking_level ("low"/"medium"/"high"); "" (unset, the default) sends no ThinkingConfig at all - some OpenAI-compatible endpoints 400 on reasoning_effort for a non-reasoning model, so this must never be forced on unconditionally (#1235).
func judgeGenConfig(maxOutputTokens int, thinkingLevel string) *genai.GenerateContentConfig {
	var cfg *genai.GenerateContentConfig
	if tc := judgeThinkingConfig(thinkingLevel); tc != nil {
		cfg = &genai.GenerateContentConfig{ThinkingConfig: tc}
	}
	if maxOutputTokens > 0 {
		if cfg == nil {
			cfg = &genai.GenerateContentConfig{}
		}
		cfg.MaxOutputTokens = int32(maxOutputTokens)
	}
	return cfg
}

// judgeThinkingConfig maps gates.judge.thinking_level to genai's enum; an
// unrecognised or empty value (the default) means "send nothing" - config
// validation is what actually rejects an unknown level.
func judgeThinkingConfig(level string) *genai.ThinkingConfig {
	switch level {
	case "low":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow}
	case "medium":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelMedium}
	case "high":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}
	default:
		return nil
	}
}

// judgeForceCloseInstruction: appended on the round's last allowed turn, or right after the judge
// repeats an identical tool call - forcedVerdictCallback has already stripped every tool (including
// submit_verdict) this turn, so the model must close with the verdict as plain JSON text; runJudgeRound's existing parseVerdict fallback picks it up exactly like a local model that skipped the tool call.
const judgeForceCloseInstruction = "\n\nSTOP - you are out of tool budget for this round; no tools, including submit_verdict, are available on this turn. " +
	"Using ONLY what you have already read and verified above, output your verdict now as a single JSON object and nothing else (no code fence, no other text): " +
	`{"score": <0-3 overall fallback>, "criteria": {"<criterion name>": {"reason": "<why>", "score": <0-3>}, ...}, "feedback": "<concise, actionable - empty if it passes>"}` +
	" Score every criterion the rubric named, from what you have already verified."

// forcedVerdictCallback strips all tools and appends judgeForceCloseInstruction on the round's last allowed turn, or the turn right after the judge repeats an identical tool call (model stutter that
// would otherwise burn the rest of the budget repeating itself, #853). forced (may be nil) is set true
// the moment tools are stripped - the round's own signal that this turn is tool-less, so callers must not offer or demand a tool call afterward (#1235: nudging submit_verdict contradicted this instruction in the same request).
func forcedVerdictCallback(maxIters int, forced *bool, receivedIDs []string) llmagent.BeforeModelCallback {
	turn := 0
	instruction := judgeForceCloseInstruction
	if len(receivedIDs) > 0 {
		// Appended to Contents (the user message), not the system prompt, so this
		// doesn't break prefix caching - but stays worded the same as the
		// round-invariant submit_verdict description for consistency.
		instruction += ` Also include a "memories" array voting on every RECALLED MEMORIES id listed above (each {"id": ..., "vote": "supported"|"contradicted"|"not_relevant", "reason": "..."}).`
	}
	return func(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		turn++
		if turn < maxIters && !repeatsLastToolCall(req.Contents) {
			return nil, nil
		}
		if forced != nil {
			*forced = true
		}
		req.Tools = nil
		if req.Config != nil {
			req.Config.Tools = nil
		}
		req.Contents = append(req.Contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: instruction}}})
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
	Memories []memoryVerdict           `json:"memories,omitempty"`
}

// lenientVerdictSchema: verdictArgs schema with every optional string property
// also accepting null. Judges intermittently send `"shortfall": null` for a
// string field (trace 9ea8cbee); strict validation rejected the whole verdict and the round ended unvetted. null unmarshals to "" so semantics are identical.
func lenientVerdictSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[verdictArgs](nil)
	if err != nil {
		return nil, err
	}
	allowNullOptionalStrings(s)
	return s, nil
}

func allowNullOptionalStrings(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	for name, p := range s.Properties {
		if p == nil {
			continue
		}
		if p.Type == "string" && !required[name] {
			p.Type, p.Types = "", []string{"null", "string"}
		}
		allowNullOptionalStrings(p)
	}
	allowNullOptionalStrings(s.Items)
	allowNullOptionalStrings(s.AdditionalProperties)
}

// newSubmitVerdictTool: builds structured-termination tool (mirrors ADK exitlooptool).
func newSubmitVerdictTool(sink *verdict, receivedIDs []string) (tool.Tool, error) {
	schema, err := lenientVerdictSchema()
	if err != nil {
		return nil, err
	}
	// Description stays round-invariant (no ids) so it never breaks the
	// system-prompt prefix cache across rounds - the ids live in the user
	// prompt's trailing receivedMemoriesSection instead.
	desc := "Record your final verdict and end the evaluation. Call this exactly once, after independently verifying the answer against every rubric criterion - and, when the prompt lists staged findings to verify, after recording a result for each one in `findings`." +
		" When the prompt lists RECALLED MEMORIES, `memories` is REQUIRED: vote on every one of them before finishing."
	return functiontool.New(functiontool.Config{
		Name:        submitVerdictTool,
		Description: desc,
		InputSchema: schema,
	}, func(ctx adkagent.Context, args verdictArgs) (map[string]any, error) {
		v := verdict{Score: args.Score, Criteria: args.Criteria, Feedback: args.Feedback, Findings: args.Findings, Memories: args.Memories}
		normalizeScale(&v)
		*sink = v
		ctx.Actions().Escalate = true
		ctx.Actions().SkipSummarization = true
		return map[string]any{"recorded": true}, nil
	})
}

// buildJudgePrompt: assembles judge's user message. Order is constitution →
// rubric → task → upstream → question → ledger → changed files → known failures → commit-hygiene evidence → answer: every section that is byte-identical round to round leads, and the one section that changes every round (the
// answer being judged) trails last, so the whole prefix ahead of it stays a prompt-cache hit across rounds instead of dying at the first volatile byte. judgePromptBuilds counts buildJudgePrompt calls - test-only seam proving fitJudgeAnswer's prompt isn't thrown away and rebuilt by runJudgeRound.
var judgePromptBuilds atomic.Int64

func buildJudgePrompt(constitution, rubric, nodeTask, upstreamAnswers string, question *genai.Content, answer, changedFiles string, act workerActivity, knownFailures string) string {
	judgePromptBuilds.Add(1)
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
	if strings.TrimSpace(upstreamAnswers) != "" {
		sb.WriteString("\n\nOUTPUT FROM UPSTREAM NODES this node's task depends on - verify claims like " +
			"\"the file/function identified above\" against this, don't take them on faith:\n")
		sb.WriteString(boundExcerpt(upstreamAnswers, maxUpstreamAnswersChars))
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
	if ev := commitHygieneEvidenceSection(nodeTask, act); ev != "" {
		sb.WriteString("\n\n")
		sb.WriteString(ev)
	}
	sb.WriteString("\n\nAnswer to judge:\n")
	sb.WriteString(answer)
	return sb.String()
}

// commitHygieneEvidenceSection: files this session wrote whose path (or basename) never appears in the
// task text - computed here, not left for the judge to re-derive, since "was this file in scope" is a
// checkable fact, not a judgement call. The judge still rules on whether the scope is JUSTIFIED (a repo-wide rename legitimately touches many files); this only hands it the list.
func commitHygieneEvidenceSection(nodeTask string, act workerActivity) string {
	var unnamed []string
	for _, p := range act.written {
		base := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			base = p[i+1:]
		}
		if !strings.Contains(nodeTask, p) && !strings.Contains(nodeTask, base) {
			unnamed = append(unnamed, p)
		}
	}
	if len(unnamed) == 0 {
		return ""
	}
	sort.Strings(unnamed)
	return "Files touched this session that the task text never named (evidence for commit_hygiene - " +
		"judge whether that scope is justified, e.g. a repo-wide rename legitimately touches many files):\n- " +
		strings.Join(unnamed, "\n- ")
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
// same as one over the whole change (#779). Zero value means "not applicable" (reviewer nodes diff the clone directly, not act.written).
type changedFilesCoverage struct {
	Scored int
	Total  int
	// Capped is true only when maxChangedFiles/changedFilesBudget cut the loop
	// short - NOT when Scored<Total merely because an individual file failed to
	// resolve/read (deleted-after-write etc.). Only the cap is "truncation"; an unrelated missing file isn't, and must not raise the note.
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

// reviewVerdictLine: surfaces the staged verdict + summary as facts for the
// judge. "" when nothing staged. The summary is included so the judge grades
// the staged record (what's actually delivered) rather than needing the worker to restate it in the chat answer just to be seen - the prompt's contract is a one-line reply after staging, and this is what backs it.
func reviewVerdictLine(act workerActivity) string {
	sd, ok := act.stagedDelivery["review"]
	if !ok || sd.Event == "" {
		return ""
	}
	out := "Staged review verdict: " + sd.Event + "\n\n"
	if strings.TrimSpace(sd.Body) != "" {
		out += "Staged review summary:\n" + sd.Body + "\n\n"
	}
	return out
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

// judgeOutputReserveTokens: fallback reply-token reserve when Config.JudgeMaxOutputTokens
// is unset; when it IS set, judgeCharBudget reserves that instead. A mismatch let prod's config (window 65536, max_output_tokens 8192) pack the prompt to
// window-2000, then ask for up to 8192 reply tokens, ~6K over the slot, truncating the judge mid-thought with no verdict (#1215 - measured a 104s stall).
const judgeOutputReserveTokens = 2_000

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
	reserve := judgeOutputReserveTokens
	if cfg.JudgeMaxOutputTokens > 0 {
		reserve = cfg.JudgeMaxOutputTokens
	}
	tokens := window - reserve
	if tokens <= 0 {
		tokens = window
	}
	return tokens * judgeCharsPerToken
}

// fitJudgeAnswer clamps answer so judge prompt fits budget, and also returns
// the full prompt it already built while measuring the fit - runJudgeRound's
// first call reuses it instead of rebuilding the identical ~244KB string. shrinkFactor < 1.0 = harder clamp for retry.
func fitJudgeAnswer(cfg Config, question *genai.Content, answer, changedFiles, knownFailures string, act workerActivity, shrinkFactor float64) (string, string) {
	budget := judgeCharBudget(cfg)
	full := buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, cfg.UpstreamAnswers, question, answer, changedFiles, act, knownFailures)
	over := len(full) - budget
	if over <= 0 && shrinkFactor >= 1.0 {
		return answer, full
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
		return answer, full
	}
	clamped := boundExcerpt(answer, target)
	return clamped, buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, cfg.UpstreamAnswers, question, clamped, changedFiles, act, knownFailures)
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
func runJudgeAgent(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer string, act workerActivity, det map[string]criterionScore, received []memory.Delivered, emit func(*genai.Part) bool) (v verdict, err error) {
	changedFiles, coverage := changedFilesSection(cfg, act)
	// Memories lead knownFailures inside this string, but buildJudgePrompt
	// still places the whole `known` blob in its volatile trailing section,
	// not the cacheable prefix - this ordering is readability only.
	known := receivedMemoriesSection(received) + judgeKnownFailuresSection(det, cfg.Threshold)
	fitted, fittedPrompt := fitJudgeAnswer(cfg, question, answer, changedFiles, known, act, 1.0)
	defer func() {
		if err == nil {
			v = applyChangedFilesCoverage(v, coverage)
		}
	}()

	var counters judgeReadCounters
	v, counters, err = judgeAttemptLoop(ctx, factory, cfg, question, fitted, changedFiles, known, fittedPrompt, act, received, emit)

	// A non-transient failure with images attached (400 on a multimodal
	// request a vision-blind/misbehaving judge model rejects) degrades to a
	// text-only retry once, rather than blocking delivery outright (#1229). q tracks that strip: once it fires, every later retry below must keep using the text-only content instead of re-attaching the images and re-triggering the same rejection (#1229 follow-up).
	q := question
	if err != nil && ctx.Err() == nil && !isTransientJudgeErr(err) && hasInlineData(question) {
		slog.Warn("judge round failed with images attached; retrying once without them",
			"component", "vetting", "agent", cfg.Agent, "chat", cfg.ChatID, "err", err)
		q = stripInlineData(question)
		v, counters, err = runJudgeRound(ctx, factory, cfg, q, fitted, changedFiles, known, "", act, received, emit)
	}

	if errors.Is(err, ErrJudgeNoVerdict) && ctx.Err() == nil {
		v, err = retryNoVerdict(ctx, factory, cfg, q, fitted, changedFiles, known, act, received, emit)
		return
	}

	if err == nil || ctx.Err() != nil {
		v = finishJudgeRound(ctx, factory, cfg, q, fitted, changedFiles, known, act, received, emit, v, counters)
		return
	}
	retryAnswer, retryPrompt := fitJudgeAnswer(cfg, q, fitted, changedFiles, known, act, 0.5)
	if retryAnswer == fitted {
		// Nothing left to shrink: return the zero verdict directly - it must never
		// reach finishJudgeRound (which assumes a real verdict to re-check).
		v = verdict{}
		return
	}
	v, _, err = runJudgeRound(ctx, factory, cfg, q, retryAnswer, changedFiles, known, retryPrompt, act, received, emit)
	return
}

// judgeAttemptLoop runs the judge round, retrying transient faults with
// exponential backoff up to judgeRetryAttempts.
func judgeAttemptLoop(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, fitted, changedFiles, known, fittedPrompt string, act workerActivity, received []memory.Delivered, emit func(*genai.Part) bool) (v verdict, counters judgeReadCounters, err error) {
	for attempt := 1; attempt <= judgeRetryAttempts; attempt++ {
		// question/fitted/changedFiles/known/act are unchanged across attempts,
		// so the prompt fitJudgeAnswer already built is still exactly right.
		v, counters, err = runJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, fittedPrompt, act, received, emit)
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
	return
}

// retryNoVerdict: a round that ran but never reached a verdict (model stutter
// exhausting the budget, #853) gets exactly one retry with a fresh session
// before surfacing unvetted - shrinking the answer wouldn't fix a stutter.
func retryNoVerdict(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, fitted, changedFiles, known string, act workerActivity, received []memory.Delivered, emit func(*genai.Part) bool) (verdict, error) {
	slog.Warn("judge ended without a verdict; retrying the round once with a fresh session",
		"component", "vetting", "agent", cfg.Agent, "chat", cfg.ChatID)
	v, counters, err := runJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, "", act, received, emit)
	if err == nil {
		v = finishJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, act, received, emit, v, counters)
	}
	return v, err
}

// finishJudgeRound: discards and re-judges once before being trusted (second
// offence accepted - one wasted round is the ceiling), for either of two
// self-inconsistent verdicts: a PASS backed by zero judge reads, or a judge-scored criterion below threshold with no `fix` (the prompt requires one only
// for a genuine failure - see inconsistentJudgeFailures). No-op otherwise.
func finishJudgeRound(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, fitted, changedFiles, known string, act workerActivity, received []memory.Delivered, emit func(*genai.Part) bool, v verdict, counters judgeReadCounters) verdict {
	v = dropUnscoredStrays(v, cfg.RubricSpecs)
	if names := unscoredCriteria(v); len(names) > 0 {
		slog.Warn("judge left rubric criteria unscored; re-judging once",
			"component", "vetting", "agent", cfg.Agent, "criteria", names)
		return reJudgeOnce(ctx, factory, cfg, question, fitted+"\n\n"+unscoredFeedback(names), changedFiles, known, act, received, emit, v)
	}
	switch {
	case unreadPass(counters.repo, v):
		slog.Warn("judge passed without reading the repo; re-judging once",
			"component", "vetting", "agent", cfg.Agent, "score", v.Score)
		return reJudgeOnce(ctx, factory, cfg, question, fitted+"\n\n"+unreadPassFeedback, changedFiles, known, act, received, emit, v)
	case unreadArtifactPass(counters.artifact, v, len(act.artifactsWritten) > 0):
		slog.Info("judge passed without reading an artifact the worker wrote; re-judging once",
			"component", "vetting", "agent", cfg.Agent, "node", cfg.NodeID, "score", v.Score, "artifacts", act.artifactsWritten)
		return reJudgeOnce(ctx, factory, cfg, question, fitted+"\n\n"+unreadArtifactPassFeedback(act.artifactsWritten), changedFiles, known, act, received, emit, v)
	default:
		if names := inconsistentJudgeFailures(v, cfg.Threshold, cfg.RubricSpecs); len(names) > 0 {
			slog.Warn("judge scored below threshold with no fix given; re-judging once",
				"component", "vetting", "agent", cfg.Agent, "criteria", names)
			return reJudgeOnce(ctx, factory, cfg, question, fitted+"\n\n"+inconsistentFailureFeedback(names), changedFiles, known, act, received, emit, v)
		}
		return v
	}
}

// reJudgeOnce re-runs the round with feedback appended to the answer, keeping
// the original verdict v if the retry itself errors - one wasted round is the
// ceiling, never an unbounded loop.
func reJudgeOnce(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, fitted, changedFiles, known string, act workerActivity, received []memory.Delivered, emit func(*genai.Part) bool, v verdict) verdict {
	v2, counters2, err2 := runJudgeRound(ctx, factory, cfg, question, fitted, changedFiles, known, "", act, received, emit)
	if err2 != nil {
		slog.Warn("re-judge failed; keeping the original verdict", "component", "vetting", "err", err2)
		return v
	}
	v2 = dropUnscoredStrays(v2, cfg.RubricSpecs)
	if unreadPass(counters2.repo, v2) {
		slog.Warn("judge passed without reading the repo again; accepting the verdict",
			"component", "vetting", "agent", cfg.Agent, "score", v2.Score)
	}
	if unreadArtifactPass(counters2.artifact, v2, len(act.artifactsWritten) > 0) {
		slog.Info("judge passed without reading an artifact again; accepting the verdict",
			"component", "vetting", "agent", cfg.Agent, "score", v2.Score)
	}
	return v2
}

// inconsistentJudgeFailures: criteria opted into RequireFixOnFail (rubric.yaml)
// that scored below threshold with an empty Fix - the rubric requires one only
// for a genuine failure of that criterion, so its absence means the score and
// the reasoning disagree, not that fix was optional. Opt-in, not a blanket
// rule: most criteria never require a named fix, and applying this check
// unconditionally to every below-threshold criterion (deterministic overrides
// like findingsGroundingCriterion included) re-asks rounds that were never
// inconsistent in the first place.
func inconsistentJudgeFailures(v verdict, threshold float64, specs map[string]criterionSpec) []string {
	var names []string
	for name, c := range v.Criteria {
		if c.Deterministic || !specs[name].RequireFixOnFail || c.Score >= threshold || strings.TrimSpace(c.Fix) != "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names) // stable order across runs (map iteration is random)
	return names
}

// inconsistentFailureFeedback: appended to the judge prompt on re-run for
// inconsistentJudgeFailures (states the mechanism, not a repeated instruction).
func inconsistentFailureFeedback(names []string) string {
	return fmt.Sprintf("Your previous verdict scored %s below the pass threshold but gave no `fix` for it - "+
		"a genuine failure always names one. Re-examine %s: if it truly fails, give the concrete fix; if your "+
		"own reasoning actually described the top band, correct the score to match it.",
		strings.Join(names, ", "), strings.Join(names, ", "))
}

// judgeRepeat* tune the runaway-generation guard shared by the judge
// and plan judge: a degenerate loop stutters far faster than any legitimate
// verdict grows, so watching a bounded trailing window is enough (#889: 18K+ tokens looped on one verdict before this existed).
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
// sizes in [minUnit, maxUnit]. Phase-aligned to the end of s rather than to fixed byte offsets, since a loop's start position is never known in advance.
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
// prebuilt, when non-"", is the exact prompt fitJudgeAnswer already built for
// this (question, answer, changedFiles, knownFailures, act) combination - callers pass "" whenever any of those differ from what produced it.
func runJudgeRound(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer, changedFiles, knownFailures, prebuilt string, act workerActivity, received []memory.Delivered, emit func(*genai.Part) bool) (verdict, judgeReadCounters, error) {
	maxIters := cfg.JudgeMaxIterations
	if maxIters <= 0 {
		maxIters = defaultJudgeMaxIterations
	}
	receivedIDs := memoryIDs(received)
	st := &judgeRoundState{cfg: cfg, maxIters: maxIters, emit: emit, ctx: ctx}
	prompt, err := judgePromptFor(ctx, cfg)
	if err != nil {
		return verdict{}, judgeReadCounters{}, err
	}
	// forcedClose is flipped by forcedVerdictCallback the instant it strips tools for
	// a forced close (#1235) - the round's own signal, not the turn counter.
	judgeAgent, counters, err := factory(prompt, &st.sink, &st.forcedClose, maxIters, cfg.JudgeMaxOutputTokens, cfg.JudgeThinkingLevel, receivedIDs, cfg.JudgeArtifactTools)
	if err != nil {
		return verdict{}, judgeReadCounters{}, fmt.Errorf("vetting: build judge agent: %w", err)
	}
	jr, err := runner.New(runner.Config{
		AppName:           "quack-judge",
		Agent:             judgeAgent,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return verdict{}, judgeReadCounters{}, fmt.Errorf("vetting: judge runner: %w", err)
	}
	st.jr = jr
	st.runCtx, st.cancel = context.WithCancel(ctx)
	defer st.cancel()
	st.sessionID = judgeSessionID(cfg.ChatID, "verdict")

	content := judgePromptContent(cfg, prebuilt, answer, changedFiles, knownFailures, act, question)
	if err := st.runTurn(content); err != nil {
		return verdict{}, counters, err
	}
	v, ok := st.verdictFrom()
	// #1259: a verdict that reached submit_verdict or the text-JSON fallback but
	// skipped the required memory votes gets the same one-shot nudge, naming the owed ids.
	owedIDs := owedMemoryVoteIDs(receivedIDs, v)
	if ok && len(owedIDs) > 0 {
		if v2, ok2, nerr := st.nudgeMemories(owedIDs); nerr != nil {
			return verdict{}, counters, nerr
		} else if ok2 {
			v, ok = v2, ok2
		}
	}
	if ok {
		return v, counters, nil
	}

	// One in-session nudge before giving up: a text turn that didn't parse as a
	// verdict is often analysis-complete/submission-wrong (#1235) - one direct ask.
	if st.nudgeAllowed() && strings.TrimSpace(st.accum.String()) != "" {
		if v, ok, nerr := st.submitNudge(); nerr != nil {
			return verdict{}, counters, nerr
		} else if ok {
			return v, counters, nil
		}
	}

	slog.Warn("judge round ended without a verdict",
		"component", "vetting", "agent", cfg.Agent, "finish_reason", string(st.lastFinish), "output_tokens", st.lastOutTokens)
	return verdict{}, counters, ErrJudgeNoVerdict
}

// judgeRoundState carries the turn state shared by the initial judge turn and the
// in-session nudges, so a nudge counts against the same maxIters budget.
type judgeRoundState struct {
	cfg           Config
	maxIters      int
	emit          func(*genai.Part) bool
	ctx           context.Context
	runCtx        context.Context
	cancel        context.CancelFunc
	jr            *runner.Runner
	sessionID     string
	sink          verdict
	forcedClose   bool
	submitted     bool
	turns         int
	accum         strings.Builder
	repeats       repeatLoopDetector
	lastFinish    genai.FinishReason
	lastOutTokens int32
	aborted       bool
}

// judgePromptContent builds the judge's user content: the (prebuilt or built) prompt
// plus the advisor-thread marker and the question's inline attachments.
func judgePromptContent(cfg Config, prebuilt, answer, changedFiles, knownFailures string, act workerActivity, question *genai.Content) *genai.Content {
	promptText := prebuilt
	if promptText == "" {
		promptText = buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, cfg.UpstreamAnswers, question, answer, changedFiles, act, knownFailures)
	}
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
	return &genai.Content{Role: "user", Parts: parts}
}

// runTurn drives one jr.Run call to completion, shared across the initial turn and the
// nudges - turns/submitted/accum/repeats carry state across all of them.
func (s *judgeRoundState) runTurn(turnContent *genai.Content) error {
	for ev, err := range s.jr.Run(s.runCtx, "judge", s.sessionID, turnContent, adkagent.RunConfig{}) {
		if err != nil {
			return err
		}
		if err := s.observeEvent(ev); err != nil {
			return err
		}
		// Safety cap: prevent infinite loop if judge never calls submit_verdict, or a
		// runaway repeat loop is decoding the same text forever (#889).
		if s.turns > s.maxIters || s.repeats.tripped {
			if s.repeats.tripped {
				slog.Warn("judge round aborted: runaway repeat detected mid-generation",
					"component", "vetting", "agent", s.cfg.Agent)
			}
			s.aborted = true
			s.cancel()
			break
		}
	}
	return nil
}

// observeEvent folds one run event into the round state; the error is a cancelled
// emit (the consumer stopped wanting parts).
func (s *judgeRoundState) observeEvent(ev *session.Event) error {
	if ev == nil {
		return nil
	}
	s.lastFinish = ev.FinishReason
	if ev.UsageMetadata != nil {
		s.lastOutTokens = ev.UsageMetadata.CandidatesTokenCount
	}
	if ev.Content == nil {
		return nil
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if err := s.scanPart(p); err != nil {
			return err
		}
	}
	if ev.TurnComplete {
		s.turns++
	}
	return nil
}

// scanPart folds one part into the round state; the error is a cancelled emit.
func (s *judgeRoundState) scanPart(p *genai.Part) error {
	switch {
	case p.FunctionCall != nil && p.FunctionCall.Name == submitVerdictTool:
		// Suppress from generic tool-call activity; success is confirmed on the matching
		// FunctionResponse below (#889).
	case p.FunctionResponse != nil && p.FunctionResponse.Name == submitVerdictTool:
		if _, failed := p.FunctionResponse.Response["error"]; !failed {
			s.submitted = true // handler ran; sink is populated
		}
	case p.Thought && p.Text != "":
		s.repeats.observe(p.Text)
		if !s.emit(stream.ThinkingPart(p.Text)) {
			return context.Canceled
		}
	case p.FunctionCall != nil:
		if !s.emit(&genai.Part{FunctionCall: p.FunctionCall}) {
			return context.Canceled
		}
	case p.FunctionResponse != nil:
		if !s.emit(&genai.Part{FunctionResponse: p.FunctionResponse}) {
			return context.Canceled
		}
	case p.Text != "":
		// Local model emits reasoning as plain text, not Thought parts.
		s.accum.WriteString(p.Text)
		s.repeats.observe(p.Text)
		if !s.emit(stream.ThinkingPart(p.Text)) {
			return context.Canceled
		}
	}
	return nil
}

// verdictFrom: a submit_verdict tool call takes priority over accum's text, since a
// later tool call always supersedes earlier unparsed text.
func (s *judgeRoundState) verdictFrom() (verdict, bool) {
	if s.submitted {
		return aggregateVerdict(s.sink), true
	}
	if v, perr := parseVerdict(s.accum.String()); perr == nil {
		return v, true
	}
	return verdict{}, false
}

// nudgeAllowed mirrors the #1235/#1236 guard shared by both nudges: a forced-close
// turn already stripped tools, and an aborted turn already cancel()ed runCtx.
func (s *judgeRoundState) nudgeAllowed() bool {
	return !s.forcedClose && !s.aborted && s.ctx.Err() == nil
}

// nudgeMemories sends the one-shot memory-vote nudge naming the owed ids (#1259).
func (s *judgeRoundState) nudgeMemories(owedIDs []string) (verdict, bool, error) {
	if !s.nudgeAllowed() {
		return verdict{}, false, nil
	}
	nudge := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: judgeMemoriesNudgeText(owedIDs)}}}
	if err := s.runTurn(nudge); err != nil {
		return verdict{}, false, err
	}
	nv, nok := s.verdictFrom()
	return nv, nok, nil
}

// submitNudge asks once, directly, for the missing submit_verdict call (#1235).
func (s *judgeRoundState) submitNudge() (verdict, bool, error) {
	nudge := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: judgeSubmitNudge}}}
	if err := s.runTurn(nudge); err != nil {
		return verdict{}, false, err
	}
	nv, nok := s.verdictFrom()
	return nv, nok, nil
}

// judgeSubmitNudge: one-shot in-session continuation when a turn ends with
// unparseable text and no submit_verdict call (#1235) - the analysis is often
// already correct and only the submission mechanism was wrong.
const judgeSubmitNudge = "You did not call submit_verdict. Call submit_verdict now with your verdict as a tool call - do not write it as text."

// ErrJudgeNoVerdict: the judge model ran - read files, spent its iteration
// budget - and never called submit_verdict. Distinct from a transport/model
// failure (the judge was never reachable at all) so the caller can tell the reader which one happened instead of calling both "unavailable" (#779).
var ErrJudgeNoVerdict = errors.New("vetting: judge ended without a verdict")

// markdownLinkRe extracts inline Markdown link targets - web URLs and local
// paths alike; only http(s) targets are scored (see citationScore).
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// citationScore: deterministic grade per cited web link, against the session ledger. Local file/code citations (e.g. "<repo>@<path>") are NOT graded here - a
// worker's own claim to have read a file quotes lines an LLM judge can check
// against the ledger directly, so a second deterministic pass produced false failures without adding coverage. Layers: fetched=1.00, searched=0.75, same host fetched=0.50, same host searched=0.25, neither=0.00. Worker-facing meaning of these tiers lives in citeReasonLegend below - keep the two in sync.
func citationScore(answer string, act workerActivity) (score float64, details []citationDetail, ok bool) {
	if len(act.fetched) == 0 && len(act.seen) == 0 {
		return 0, nil, false
	}
	fetchedURL, fetchedHost := normalizedSets(slices.Collect(maps.Keys(act.fetched)))
	seenURL, seenHost := normalizedSets(slices.Collect(maps.Keys(act.seen)))

	dedup := make(map[string]struct{})
	var sum float64
	for _, m := range markdownLinkRe.FindAllStringSubmatch(answer, -1) {
		if len(m) < 2 {
			continue
		}
		target := strings.TrimSpace(m[1])
		s, ok := linkBackingScore(target, dedup, fetchedURL, seenURL, fetchedHost, seenHost)
		if !ok {
			continue
		}
		details = append(details, citationDetail{url: target, score: s})
		sum += s
	}
	if len(details) == 0 {
		return 0, nil, false
	}
	return sum / float64(len(details)), details, true
}

// linkBackingScore: one cited link's deterministic backing tier -
// fetched URL > seen URL > fetched host > seen host > unbacked. ok=false when
// the target isn't web-gradeable or is a duplicate (dedup is mutated).
func linkBackingScore(target string, dedup map[string]struct{}, fetchedURL, seenURL, fetchedHost, seenHost map[string]bool) (s float64, ok bool) {
	u, err := url.Parse(target)
	if err != nil || target == "" {
		return 0, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, false // mailto:, local path, in-document anchor, … - not web-gradeable
	}
	norm, host := normalizeURL(target)
	if norm == "" {
		return 0, false
	}
	if _, dup := dedup[norm]; dup {
		return 0, false
	}
	dedup[norm] = struct{}{}
	switch {
	case fetchedURL[norm]:
		s = 1.00
	case seenURL[norm]:
		s = 0.75
	case host != "" && fetchedHost[host]:
		s = 0.50
	case host != "" && seenHost[host]:
		s = 0.25
	}
	return s, true
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
		for _, u := range slices.Sorted(maps.Keys(act.fetched)) {
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
	// maxUpstreamAnswersChars is fixed regardless of JudgeContextWindow; on a
	// small window fitJudgeAnswer's minJudgeAnswerChars floor absorbs the pressure by clamping the answer, never this section.
	maxUpstreamAnswersChars = 24_000
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

// reviseReplyRule closes every revise request: an edited artifact is the deliverable, so
// re-typing it as the reply cost prod chat cedfc299 26k output tokens (6 min) in one round.
const reviseReplyRule = "If you edited an artifact, your reply is a short note of what changed that names the artifact: the judge and every later step read the artifact itself, so repeating it in the reply only costs time. " +
	"Otherwise output only the corrected answer with no preamble or commentary.\n\n"

// buildRevisionContent: re-invokes worker to address judge feedback. Every section bounded (boundExcerpt).
// #941: the worker gets the structured verdict envelope (definition/bands/anchor per
// failing criterion), not prose - this is what closes the gap where the worker previously had no rubric access at all. Rubric text itself isn't a parameter: applyRubricSpecs (node.go) already folds each failing criterion's parsed definition/bands into env before this runs.
func buildRevisionContent(constitution string, question *genai.Content, answer string, env verdictEnvelope, act workerActivity, citationOnly bool, notes []JudgeNote) *genai.Content {
	var sb strings.Builder
	// Stable-first (finding 3): the original question, byte-identical to what the draft
	// round sent (same boundExcerpt, no header), leads every round so the prefix cache
	// carries draft -> revise -> revise instead of dying at the volatile verdict.
	sb.WriteString(boundExcerpt(contentPlainText(question), maxOriginalQuestionChars))
	sb.WriteString("\n\n")
	if citationOnly {
		// The answer's substance passed; only citation-form criteria failed. This is a
		// formatting pass, not re-research: the worker already fetched the URLs
		// (listed in the activity section below), so re-fetching them wastes tokens and time. Tell it to attach what it has.
		sb.WriteString("Your previous answer is substantively fine - the ONLY problem is missing inline citations. " +
			"You already retrieved the sources listed below (URLs you fetched and searched); attach them inline as Markdown links to the claims they support, in the same sentence or bullet as each figure, and remove any figure none of them states - edit_artifact the artifact if the answer lives in one. " +
			"Do NOT re-fetch or search again - this is purely a citation-formatting fix. " + reviseReplyRule)
	} else {
		sb.WriteString("An independent reviewer evaluated your previous answer and it must be improved before it can be returned. " +
			"Below is the structured verdict: each failing criterion's definition, scoring bands, and (where locatable) an anchor into your answer, plus a concrete fix. " +
			"Address every failure - use your tools to fix the gaps: re-fetch and verify sources, correct or remove unsupported claims, add missing citations. " +
			"If you wrote any artifact last round (list_artifacts shows your prior revision), read_artifact it and edit_artifact the specific parts this verdict flags - " +
			"don't regenerate it from scratch. write_<kind> replaces a whole artifact in one reply, and a reply's output (reasoning included) is capped, so a full rewrite only fits a short artifact. " +
			reviseReplyRule)
	}
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Verdict:\n```json\n")
	sb.WriteString(boundExcerpt(marshalEnvelope(env), maxFeedbackChars))
	sb.WriteString("\n```\n\n")
	// Notes source from the persisted judge_round record (#1092), each
	// anchored to the exact prior-round artifact revision it concerns - so
	// the worker can read_artifact/edit_artifact that revision directly instead of re-deriving what changed from prose alone.
	if len(notes) > 0 {
		sb.WriteString("Notes (read/edit the exact artifact revision each one references):\n```json\n")
		sb.WriteString(boundExcerpt(marshalNotes(notes), maxFeedbackChars))
		sb.WriteString("\n```\n\n")
	}
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Your previous answer:\n")
	sb.WriteString(boundExcerpt(answer, maxPreviousAnswerChars))
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// buildFinalizeContent: asks worker to write final answer when round 0 ended without one.
func buildFinalizeContent(question *genai.Content, act workerActivity) *genai.Content {
	var sb strings.Builder
	// Stable-first (finding 3): same reasoning as buildRevisionContent.
	sb.WriteString(boundExcerpt(contentPlainText(question), maxOriginalQuestionChars))
	sb.WriteString("\n\n")
	sb.WriteString("A response of 0 length was received. If you have finished your research, " +
		"do not call any more tools - just write your complete response again now, using everything " +
		"you found above. If you are not done with your research, there was likely an error: please " +
		"make sure you close all of your reasoning/thinking and tool-call blocks and continue " +
		"your research. Output only the answer with no preamble or commentary.\n\n")
	if section := buildActivitySection(act); section != "" {
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n")
	}
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// continuationMarker: signals that this turn continues an unfinished task.
const continuationMarker = "CONTINUE THE TASK - it is not finished."

// buildContinuationPrompt: tool-bearing continuation directive (do the remaining work, not a write-up).
func buildContinuationPrompt(task string, act workerActivity, checks []string, readOnly, hasDeliverTarget, isReviewer, existingPR bool) string {
	var sb strings.Builder
	// Stable-first (finding 3): same reasoning as buildRevisionContent.
	sb.WriteString(boundExcerpt(task, maxOriginalQuestionChars))
	sb.WriteString("\n\n")
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
	return sb.String()
}

// judgeScaleMax: the rubric's raw integer scale (0/1/2/3 - see rubric.yaml
// scale.max across every agent bundle). Shared here because normalizeScale
// converts raw judge output to the internal 0-1 axis by this divisor.
const judgeScaleMax = 3

// normalizeScale: converts the judge's raw 0-judgeScaleMax rubric scale to
// the internal 0-1 axis. Idempotent.
func normalizeScale(v *verdict) {
	if verdictAlreadyNormalized(v) {
		return
	}
	v.Score /= judgeScaleMax
	for name, c := range v.Criteria {
		c.Score /= judgeScaleMax
		v.Criteria[name] = c
	}
}

// verdictAlreadyNormalized reports whether every score in v is a fraction
// strictly between 0 and 1 - the signature of a model that ignored the
// integer-scale instruction and answered directly on 0-1. A whole-number score is always treated as raw scale, even when it equals 1, since 1 is itself a legal raw level (do not conflate it with a legacy "full marks").
func verdictAlreadyNormalized(v *verdict) bool {
	found := false
	check := func(s float64) bool {
		if s < 0 || s >= 1 {
			return false
		}
		if s != 0 {
			found = true
		}
		return true
	}
	if !check(v.Score) {
		return false
	}
	for _, c := range v.Criteria {
		if !check(c.Score) {
			return false
		}
	}
	return found
}

// inferAnchorKind fills a missing anchor kind from whichever payload field is
// set (judges near-miss the schema by omitting it, trace 9ea8cbee). Ambiguous
// or empty anchors are left as-is; sanitizeAnchors drops them later.
func inferAnchorKind(a *anchorSpec) {
	if a == nil || a.Kind != "" {
		return
	}
	switch {
	case a.Text != "" && a.Path == "" && a.Expected == "":
		a.Kind = "quote"
	case a.Path != "" && a.Text == "" && a.Expected == "":
		a.Kind = "path"
	case a.Expected != "" && a.Text == "" && a.Path == "":
		a.Kind = "omission"
	}
}

// dropCriteria removes the named criteria and recomputes the weakest-link
// score over what's left (#1092: a fan-out slice reviewer isn't judged on
// structured_verdict, since it never owns the delivered verdict).
func dropCriteria(v verdict, names ...string) verdict {
	for _, name := range names {
		delete(v.Criteria, name)
	}
	lowest := 1.0
	for _, c := range v.Criteria {
		if c.Score < lowest {
			lowest = c.Score
		}
	}
	if len(v.Criteria) > 0 {
		v.Score = lowest
	}
	return v
}

// aggregateVerdict: weakest-link gating - lowest criterion is the overall score. Clamped [0,1].
func aggregateVerdict(v verdict) verdict {
	applyFindingsVerdict(&v)
	// #941: reason -> shortfall migration, single choke point for every path
	// that produces a verdict (submit_verdict tool and the text-fallback parser alike).
	for name, c := range v.Criteria {
		if strings.TrimSpace(c.Shortfall) == "" && strings.TrimSpace(c.Reason) != "" {
			c.Shortfall = c.Reason
			v.Criteria[name] = c
		}
		inferAnchorKind(c.Anchor)
	}
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

// UnmarshalJSON marks an entry with no score key as Unscored; both the text
// fallback and the submit_verdict tool decode criteria through encoding/json.
func (c *criterionScore) UnmarshalJSON(b []byte) error {
	type plain criterionScore
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) == nil {
		_, scored := keys["score"]
		p.Unscored = !scored
	}
	*c = criterionScore(p)
	return nil
}

// dropUnscoredStrays removes unscored entries the rubric does not define: a
// judge's aside such as "score_note", which would otherwise fail the round as a 0.
func dropUnscoredStrays(v verdict, specs map[string]criterionSpec) verdict {
	if len(specs) == 0 {
		return v
	}
	for name, c := range v.Criteria {
		if _, known := specs[name]; c.Unscored && !known {
			slog.Warn("judge returned an unscored key that is not a rubric criterion; dropping it", "component", "vetting", "key", name)
			delete(v.Criteria, name)
		}
	}
	return aggregateVerdict(v)
}

// unscoredCriteria: judge criteria the verdict names but gives no score.
func unscoredCriteria(v verdict) []string {
	var names []string
	for name, c := range v.Criteria {
		if c.Unscored && !c.Deterministic {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// unscoredFeedback: appended to the judge prompt on the re-run for unscoredCriteria.
func unscoredFeedback(names []string) string {
	return fmt.Sprintf("Your previous verdict gave %s no score, so it counted as 0. Score %s on the rubric's scale.",
		strings.Join(names, ", "), strings.Join(names, ", "))
}

// emitEvaluationResults: records gen_ai.evaluation.result event per criterion. runID stands in for responseID.
func emitEvaluationResults(ctx context.Context, responseID string, v verdict) {
	names := make([]string, 0, len(v.Criteria))
	for name := range v.Criteria {
		names = append(names, name)
	}
	sort.Strings(names) // map iteration is random; a stable emit order keeps recorded-vs-new diffs meaningful
	// gen_ai.agent.name: EmitLog stamps session/node/round only.
	agent := ledger.CoordsFromContext(ctx).Agent
	for _, name := range names {
		cs := v.Criteria[name]
		attrs := []attribute.KeyValue{
			attribute.String(otelobs.GenAIResponseID, responseID),
			attribute.String(otelobs.GenAIEvaluationName, name),
			attribute.Float64(otelobs.GenAIEvaluationScore, cs.Score),
			attribute.String(otelobs.GenAIEvaluationExplain, cs.Reason),
		}
		if agent != "" {
			attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, agent))
		}
		otelobs.EmitLog(ctx, judgeScope, "", attrs...)
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
		Memories []memoryVerdict            `json:"memories,omitempty"` // #1259: text-JSON fallback also carries votes
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
	v := verdict{Score: rv.Score, Passed: rv.Passed, Feedback: feedback, Memories: rv.Memories}

	// Decode per-criterion entries, skipping non-object values. When score,
	// passed, or feedback ended up inside criteria, recover them explicitly.
	for name, entry := range rv.Criteria {
		var cs criterionScore
		if err := json.Unmarshal(entry, &cs); err != nil {
			switch name {
			case "feedback":
				json.Unmarshal(entry, &v.Feedback) //nolint:errcheck // best-effort: the primary unmarshal already failed, this is a shape-specific retry
			case "passed":
				json.Unmarshal(entry, &v.Passed) //nolint:errcheck // best-effort: the primary unmarshal already failed, this is a shape-specific retry
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
func runWriterFresh(ctx context.Context, m model.LLM, content *genai.Content, chatID string) (string, error) {
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
	for ev, rerr := range wr.Run(ctx, "writer", judgeSessionID(chatID, "finalize"), content, adkagent.RunConfig{}) {
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
