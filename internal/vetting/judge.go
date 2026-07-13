package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

const (
	// submitVerdictTool is the name of the structured-termination tool the
	// agentic judge calls to record its verdict and end its run.
	submitVerdictTool = "submit_verdict"

	// defaultJudgeMaxIterations bounds the judge's agentic tool loop (model
	// turns per round) when Config.JudgeMaxIterations is unset. It is high
	// enough for a code-quality judge to open a handful of changed files
	// (read_file/grep/list_dir each cost one model turn) AND load a review skill
	// (list_skills + load_skill cost a turn or two more) before it calls
	// submit_verdict; a tool-less research judge still submits on turn one, so
	// the extra headroom is free there. The cap only exists so a judge that never
	// calls submit_verdict can't loop forever.
	defaultJudgeMaxIterations = 14

	// judgeBehaviour* compose the behaviour layer of the agentic judge's system
	// prompt (promptbuilder.Judge wraps it with identity, tools, and environment
	// layers, exactly like a specialist agent's prompt.md). The middle clause
	// varies with whether the judge was wired with read-only workspace tools;
	// judgeBehaviour(hasReadTools, hasSkills) picks it and appends the skills
	// clause when the judge holds the skill toolset. Either way it terminates by
	// calling submit_verdict — never by emitting JSON text. Per-criterion
	// reason-before-score (G-Eval) keeps the scoring disciplined; the caller
	// re-derives the overall score as the lowest criterion in aggregateVerdict.
	judgeBehaviourHead = "You did NOT write the answer being evaluated, and you must not trust its assertions. "

	// judgeNoToolsClause is the middle clause for a tool-less judge (pure-research
	// deployments with no workspace jail): it scores on the answer's own merits.
	judgeNoToolsClause = "You have no tools. Judge the answer on its own merits against the rubric. "

	// judgeReadToolsClause is the middle clause when the judge holds the read-only
	// workspace tools (read_file, list_dir, glob, grep). It tells the judge to
	// OPEN the real artifacts and ground code-quality scores in them rather than
	// the worker's self-report, while staying bounded and strictly read-only.
	judgeReadToolsClause = "You have read-only workspace tools: read_file, list_dir, glob, grep. When the answer involves code or other workspace work, USE them to ground your quality scores in the real artifacts rather than trusting the worker's self-report — the workspace ledger below lists the paths the worker touched, so read_file those files, grep for patterns, and list_dir to inspect structure, then judge the ACTUAL code (correctness, edge cases, test thoroughness, matching the repo's existing conventions, the logic/render split) instead of the answer's description of it. These tools are STRICTLY read-only: you cannot and must not modify, create, delete, or run anything in the workspace. Read only what you need to reach a verdict — inspect the changed files, do not spelunk the whole tree. For a pure-research answer with no workspace work you won't need them. "

	// judgeSkillsClause is appended when the judge holds the skill toolset
	// (list_skills / load_skill / load_skill_resource — the SAME tools the worker
	// had). Rather than statically baking review principles into this prompt, the
	// judge can agentically pull up whatever quality/review skill is relevant per
	// case and score against the same principles the worker was told to follow, so
	// its judgment auto-reflects the current skill library as skills evolve.
	// Deliberately optional and bounded: the judge uses judgment, need not load a
	// skill every time, and still terminates with exactly one submit_verdict.
	judgeSkillsClause = "You also have skill tools (list_skills, load_skill, load_skill_resource) — the same skills the worker could use. When it helps, load a relevant review or quality skill (for example a code-review skill like `ponytail-review`, or call list_skills to see what is available) so you can ground your quality assessment in the SAME principles the worker was told to follow, rather than principles baked into this prompt. This is OPTIONAL and bounded: use your judgment, do not load a skill on every case, load at most what you need to reach a verdict, and still finish with exactly one submit_verdict call. "

	judgeBehaviourTail = "If an image is attached to this message, you can see it — use it to directly verify any visual claims in the answer. If there is no image, judge on internal consistency and appropriate hedging only; do NOT penalise an answer merely because you cannot see the source. " +
		"Do NOT try to verify which URLs were fetched — citation backing is checked separately by deterministic code, so score `cites_sources` only on whether claims carry followable links at all, not on whether you think a URL is real. " +
		"CRITICAL: the agent retrieved live web content that you do not have, and your own world knowledge is stale and incomplete. NEVER treat a claim as fabricated or ungrounded merely because you do not recognize it, it sounds new, or it postdates your training — an unfamiliar title, name, product, or event is NOT evidence of fabrication. A specific is 'invented' only when the answer's OWN text is internally inconsistent or makes a precise claim it never supports, never because it conflicts with your memory. When a claim carries an inline citation, treat it as grounded. " +
		"Score EVERY criterion the rubric names — no more, no fewer. For each, reason in one or two sentences, then assign an INTEGER score from 0 to 10 using the rubric's scoring bands (10 = the criterion is fully met, 0 = total failure; use the intermediate values for partial quality — do not snap to 0, 5, or 10). Judge substance, not style: length and fluent prose earn no credit. Each criterion is an independent requirement: the answer's overall score is its WEAKEST criterion, so a single failing criterion sinks it — do not let a strong dimension excuse a failing one. " +
		"When — and only when — you have scored every criterion, call the submit_verdict tool exactly once with: `criteria` (an object mapping each criterion name to {reason, score}), `score` (the lowest of your criterion scores), and `feedback` (concrete, actionable notes naming the lowest-scoring criteria and what to fix; empty when the answer passes). " +
		"Do NOT write the verdict as prose or JSON in your reply — calling submit_verdict is the only way to finish."
)

// judgeBehaviour assembles the judge's behaviour prompt: the middle clause is
// selected by whether the judge was wired with read-only workspace tools, and
// the skills clause is appended when the judge holds the skill toolset.
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

// criterionScore is the judge's per-criterion assessment in a G-Eval verdict.
// The judge scores each criterion on the rubric's 0–10 integer scale; the score
// is normalised to 0.0–1.0 at capture (see normalizeScale) so the rest of the
// pipeline — deterministic criteria, caps-free aggregation, the threshold — all
// work on a single 0–1 axis.
type criterionScore struct {
	Reason string  `json:"reason,omitempty"`
	Score  float64 `json:"score"`
}

// verdict is the judge's structured score for one round. When Criteria is
// populated, aggregateVerdict sets Score to the LOWEST criterion (weakest-link
// gating) rather than the judge's holistic value — there is no averaging and no
// hard caps, so a single failing criterion sinks the answer on its own.
type verdict struct {
	Criteria map[string]criterionScore `json:"criteria,omitempty"`
	Score    float64                   `json:"score"`
	Passed   bool                      `json:"passed"`
	Feedback string                    `json:"feedback"`
}

// JudgeFactory builds a fresh agentic judge bound to sink: when the judge calls
// the submit_verdict tool, its arguments are written into sink. A new judge is
// built per round so each round's submit_verdict binds a clean sink. The factory
// closes over the judge model and the judge's read-only workspace tools (built
// ONCE — they're jailed at construction and carry no per-round state); see
// NewJudgeFactory.
type JudgeFactory func(sink *verdict) (adkagent.Agent, error)

// NewJudgeFactory returns a JudgeFactory that builds the agentic judge as an ADK
// llmagent with judgeModel, the supplied read-only workspace tools (read_file,
// list_dir, glob, grep — so the judge can OPEN the files a coding worker wrote
// and score code quality from the real source, not the worker's self-report),
// the supplied skill toolsets (load_skill / list_skills / load_skill_resource —
// the SAME skills the worker had, so the judge can pull up a relevant quality/
// review skill and score against the same principles the worker followed), and a
// per-round submit_verdict tool bound to the caller's sink. Pass no read tools
// and no skillsets for a pure-research deployment: the judge then scores one-shot
// from the answer text alone. Passing nil/empty for either behaves exactly as if
// that capability were absent. The read tools MUST be read-only — never wire
// write_file, edit_file, delete_path, git_*, or run_command; the judge must not
// mutate the workspace or run anything. Skill lookups are read-only content
// fetches, so they are safe in the judge's isolated runner.
func NewJudgeFactory(judgeModel model.LLM, readTools []tool.Tool, skillsets []tool.Toolset) JudgeFactory {
	behaviour := judgeBehaviour(len(readTools) > 0, len(skillsets) > 0)
	return func(sink *verdict) (adkagent.Agent, error) {
		submit, err := newSubmitVerdictTool(sink)
		if err != nil {
			return nil, err
		}
		judgeTools := make([]tool.Tool, 0, len(readTools)+1)
		judgeTools = append(judgeTools, readTools...)
		judgeTools = append(judgeTools, submit)
		return llmagent.New(llmagent.Config{
			Name:        "judge",
			Description: "independent skeptical verifier",
			Model:       judgeModel,
			InstructionProvider: func(_ adkagent.ReadonlyContext) (string, error) {
				return promptbuilder.Judge(judgeTools, behaviour), nil
			},
			Tools:    judgeTools,
			Toolsets: skillsets,
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
	}, func(ctx adkagent.Context, args verdictArgs) (map[string]any, error) {
		v := verdict{Score: args.Score, Criteria: args.Criteria, Feedback: args.Feedback}
		normalizeScale(&v)
		*sink = v
		ctx.Actions().Escalate = true
		ctx.Actions().SkipSummarization = true
		return map[string]any{"recorded": true}, nil
	})
}

// buildJudgePrompt is the user message handed to the agentic judge: the
// constitution + rubric, the question, the worker's WORKSPACE ledger (when it
// performed any fs/git/run_command operations — see ledger.go), and the answer
// to judge. The judge does NOT see the worker's web retrieval, so the rubric
// tells it to judge grounding by the presence of inline citations and never to
// treat unfamiliar/recent cited facts as fabricated — its own world knowledge
// is stale. Workspace operations are different: the ledger IS ground truth
// (reconstructed from session events, not from the worker's narration), so a
// claims_match_activity rubric criterion can hard-fail an answer asserting an
// operation the ledger doesn't contain (the live-e2e fabricated-commit hole).
func buildJudgePrompt(constitution, rubric, nodeTask string, question *genai.Content, answer, changedFiles string, act workerActivity) string {
	var sb strings.Builder
	if constitution != "" {
		sb.WriteString("Principles:\n")
		sb.WriteString(constitution)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Scoring rubric:\n")
	sb.WriteString(rubric)
	// Score the node against ITS OWN task. The question below is the worker's full
	// prompt, which carries the user's whole request as background (dag.buildTask) —
	// and in a plan that ends in "commit, push, open a PR", judging a READ-ONLY node
	// against that fails it for work that was never its to do and is already assigned
	// to a sibling. The continuation loop made exactly this mistake and left every
	// explorer unfinishable (see node.go); the judge must not repeat it one stage later.
	if strings.TrimSpace(nodeTask) != "" {
		sb.WriteString("\n\nWHAT YOU ARE SCORING — this node's own task, and nothing else:\n")
		sb.WriteString(nodeTask)
		sb.WriteString("\n\nThe request below is BACKGROUND: it is the whole job, most of which belongs to " +
			"OTHER nodes. Do NOT penalise this node for work the task above does not ask of it — a read-only " +
			"research node that committed no code has not failed; that was never its job.")
	}
	sb.WriteString("\n\nUser's question:\n")
	sb.WriteString(questionText(question))
	if ws := buildWorkspaceSection(act); ws != "" {
		sb.WriteString("\n\n")
		sb.WriteString(ws)
	}
	if changedFiles != "" {
		sb.WriteString("\n\n")
		sb.WriteString(changedFiles)
	}
	sb.WriteString("\n\nAnswer to judge:\n")
	sb.WriteString(answer)
	return sb.String()
}

// changedFilesBudget caps the TOTAL bytes of changed-file content injected into
// the judge prompt, and perFileBudget the slice of any single file — enough for
// the judge to see the actual deliverable (a game's page + logic + tests) but
// bounded so a large diff can't blow the judge's window. The judge still holds
// read tools for anything it wants beyond this window.
const (
	changedFilesBudget = 24000
	perFileBudget      = 8000
	maxChangedFiles    = 12
)

// buildChangedFilesSection re-reads the files the worker actually wrote/edited
// (act.written) off disk through the SAME jail its tools used and formats them
// for the judge prompt, so a judge that (being a small model) won't tool-call
// still scores the REAL post-edit source instead of the worker's self-report —
// the fix for the 2026-07-12 run where a blind judge passed an incomplete,
// non-compiling deliverable it never opened. Returns "" when there are no
// written files or no jail is wired (pure-research nodes, unjailed deployments).
// Best-effort: a path that fails to resolve/read is skipped, degrading to
// today's no-injection behaviour rather than erroring.
//
// chatID is the per-chat workspace scope (the run's chat/session id) the worker
// actually wrote under (<root>/<userID>/<chatID>/<rel>) — WITHOUT it the judge
// would re-read from the per-user root where nothing was written, silently
// no-op'ing this whole "judge reads the real files" fix. "" falls back to the
// per-user root (unjailed/legacy — see Jail.Resolve).
func buildChangedFilesSection(act workerActivity, jail *workspace.Jail, userID, chatID string) string {
	if len(act.written) == 0 || jail == nil {
		return ""
	}
	var sb strings.Builder
	total := 0
	shown := 0
	for _, rel := range act.written {
		if shown >= maxChangedFiles || total >= changedFilesBudget {
			break
		}
		abs, err := jail.Resolve(userID, chatID, rel)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue // deleted-after-write, moved, or unreadable — skip
		}
		body := boundExcerpt(string(raw), perFileBudget)
		if rem := changedFilesBudget - total; len(body) > rem {
			body = boundExcerpt(body, rem)
		}
		if shown == 0 {
			sb.WriteString("ACTUAL CURRENT CONTENT OF THE FILES THE WORKER CREATED/CHANGED (read these; do not trust the answer's description of them — judge the code that is really on disk: does the deliverable actually build, is it complete, does it match the repo's conventions, are the tests real):\n")
		}
		fmt.Fprintf(&sb, "\n----- %s -----\n", rel)
		sb.WriteString(body)
		sb.WriteString("\n")
		total += len(body)
		shown++
	}
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
func runJudgeAgent(ctx context.Context, factory JudgeFactory, cfg Config, question *genai.Content, answer string, act workerActivity, emit func(*genai.Part) bool) (verdict, error) {
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

	changedFiles := buildChangedFilesSection(act, cfg.Workspace, cfg.WorkspaceUserID, cfg.ChatID)
	parts := []*genai.Part{{Text: buildJudgePrompt(cfg.Constitution, cfg.Rubric, cfg.Task, question, answer, changedFiles, act)}}
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

// markdownLinkRe extracts inline Markdown link targets — web URLs AND local
// paths ([games-repo/app/games.ts](games-repo/app/games.ts)): repo-exploration
// and coding nodes cite the files they read, not web pages.
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// citationScore deterministically grades how well each cited link target in
// the answer is backed by what the worker actually retrieved this session — no
// model involved, so it can't "reason wrong" about a string match the way a
// small judge model does. Each cited web URL is scored in layers:
//
//	exact URL fetched   → 1.00   (the worker read this exact page)
//	at/under a cloned repo → 1.00 (the whole repo is on local disk — every
//	                              blob/tree/file link under it is retrieved
//	                              material by construction)
//	exact URL searched  → 0.75   (this exact URL appeared in search results)
//	same host fetched   → 0.50   (a different page on this host was fetched)
//	same host searched  → 0.25   (the host showed up in search results)
//	neither             → 0.00   (the worker never encountered this URL or host)
//
// A cited LOCAL path (a link target with no scheme) is fully backed (1.00) when
// the ledger saw it (read/write/edit/delete) or it lies under a git_clone'd
// dir, else unbacked (0.00) — a path the worker never touched is as fabricated
// as a URL it never encountered. Non-web schemes (mailto:) and pure in-document
// anchors are skipped: not citations we can grade.
//
// URLs are normalized (lowercased scheme+host, fragment dropped, trailing slash
// trimmed) before matching so cosmetic differences don't cost points. The
// returned score is the mean across distinct cited targets; details carries the
// per-target breakdown for logging/feedback. ok is false when the answer cites
// nothing gradeable (caller decides how to treat an uncited answer).
func citationScore(answer string, act workerActivity) (score float64, details []citationDetail, ok bool) {
	// No retrieval or workspace-path activity recorded (a non-web agent like the
	// synthesizer, which re-cites URLs from its upstream inputs) → we can't grade
	// backing, so don't override; leave the model's cites_sources judgment in place.
	if len(act.fetched) == 0 && len(act.seen) == 0 && len(act.clonedRepos) == 0 && len(act.paths) == 0 {
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
			continue // mailto:, ftp:, … — not a gradeable citation
		default:
			// Local path. u.Path drops a fragment ([x](file.md#L4)) and query.
			np := normalizePath(u.Path)
			if np == "" {
				continue // pure in-document anchor like (#section)
			}
			key = np
			if act.paths[np] || underClonedDir(np, act.clonedDirs) {
				s = 1.00
			}
		}
		if _, dup := dedup[key]; dup {
			continue
		}
		dedup[key] = struct{}{}
		details = append(details, citationDetail{url: target, score: s})
		sum += s
	}
	if len(details) == 0 {
		return 0, nil, false
	}
	return sum / float64(len(details)), details, true
}

// normalizedCloneURLs normalizes clone URLs for prefix matching: normalizeURL
// plus a trailing ".git" trim, so https://host/org/repo.git backs a cite of
// https://host/org/repo/blob/main/README.md.
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

// underClonedRepo reports whether a normalized cited URL points AT or UNDER a
// cloned repo. Segment-aware: the "/" appended to the clone URL means a clone
// of …/org/repo does NOT back …/org/repo-two.
func underClonedRepo(norm string, cloneNorms []string) bool {
	n := strings.TrimSuffix(norm, ".git")
	for _, c := range cloneNorms {
		if n == c || strings.HasPrefix(n, c+"/") {
			return true
		}
	}
	return false
}

// normalizePath trims the cosmetic variance in a workspace-relative path
// ("./x", trailing "/"). Known ceiling: pure string normalization — no
// symlink/".." resolution, and a repo-relative cite ("app/x.ts") does not
// match its clone-dir-prefixed ledger entry ("repo/app/x.ts"). Good enough in
// practice: workers cite the same path string they passed to read_file.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return strings.TrimRight(p, "/")
}

// underClonedDir reports whether a normalized local path is one of, or lies
// under, a git_clone'd dir (segment-aware, like underClonedRepo). Every file
// in a cloned repo is retrieved material by construction.
func underClonedDir(np string, cloneDirs []string) bool {
	for _, d := range cloneDirs {
		if np == d || strings.HasPrefix(np, d+"/") {
			return true
		}
	}
	return false
}

// citationDetail is one cited link target's (URL or local path) deterministic
// backing score.
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

// buildActivitySection returns a prompt section summarising what retrieval the
// worker performed. An empty return means no retrieval happened (non-web agent
// or a session where all fetches failed) — the section is omitted entirely.
func buildActivitySection(act workerActivity) string {
	if len(act.searches) == 0 && len(act.fetched) == 0 && len(act.workspace) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(act.searches) > 0 || len(act.fetched) > 0 {
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
	}
	// Workspace ledger (ledger.go): the fs/git/run_command operations the
	// worker actually performed — in the revise prompt it reminds the worker
	// what it has (and has NOT) already done, so a revision doesn't repeat the
	// original's claimed-but-never-performed operations.
	if ws := buildWorkspaceSection(act); ws != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(ws)
	}
	return sb.String()
}

// Caps on the individually-unbounded sections of the revise / finalize prompt.
// That prompt becomes contents[0] of the worker's next request, which context
// compaction structurally CANNOT touch (summarisation leaves contents[0]
// verbatim) — so an unbounded revise prompt is the one input that can push a
// request past the model window with no recovery, spiral into an empty draft →
// tool-less recovery → judge 0 → a bigger revise prompt, and strand the node
// (live failure 2026-07-12: an implement node whose revise prompt inlined the
// full upstream explore-repo report + the previous answer verbatim). A coding
// worker has the repo on disk and its own session/tools, so a bounded excerpt
// (head+tail, with a marker) carries the intent without a 20k-token replay.
const (
	maxOriginalQuestionChars = 24_000 // ~6k tokens: the task prompt (embeds upstream node outputs)
	maxPreviousAnswerChars   = 16_000 // ~4k tokens: the worker's prior draft
	maxActivitySectionChars  = 32_000 // ~8k tokens: retrieval list + workspace ledger
	maxFeedbackChars         = 16_000 // ~4k tokens: judge narrative + a failing check's output tail
)

// boundExcerpt keeps s's head and tail around a loud truncation marker when it
// exceeds maxChars, so no single section can blow the revise/finalize prompt
// (contents[0]) past the model window. Head+tail preserves both the opening
// framing and the closing detail (a task's acceptance criteria, a ledger's most
// recent ops, a test log's final failure). Input within the cap passes through
// untouched. Slightly favours the head (task framing) at 60/40.
func boundExcerpt(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	const marker = "\n\n…[excerpt truncated to fit the context window — the full material is in your own session/tools; re-read the files if you need more detail]…\n\n"
	keep := maxChars - len(marker)
	if keep <= 0 {
		return strings.ToValidUTF8(s[:maxChars], "")
	}
	head := keep * 3 / 5
	return strings.ToValidUTF8(s[:head], "") + marker + strings.ToValidUTF8(s[len(s)-(keep-head):], "")
}

// buildRevisionContent constructs the user message for the agentic, session-
// continuing revision: the worker is re-invoked (continuing its own session and
// tools) to address the judge's feedback, then output only the corrected answer.
// It mirrors buildCritiqueContent but is driven by the reviewer's feedback rather
// than a generic self-critique. Every embedded section is bounded (boundExcerpt)
// so this prompt — the next request's uncompactable contents[0] — can't overflow
// the model window (see the cap constants above).
func buildRevisionContent(constitution string, question *genai.Content, answer, feedback string, act workerActivity, citationOnly bool) *genai.Content {
	var sb strings.Builder
	if citationOnly {
		// The answer's substance passed; only cites_sources failed. This is a
		// formatting pass, not re-research: the worker already fetched the URLs
		// (listed in the activity section below), so re-fetching them wastes
		// tokens and time. Tell it to attach what it has.
		sb.WriteString("Your previous answer is substantively fine — the ONLY problem is missing inline citations. " +
			"You already retrieved the sources listed below (URLs you fetched and searched); attach them inline as Markdown links to the claims they support. " +
			"Do NOT re-fetch or search again — this is purely a citation-formatting fix. " +
			"Then output only the corrected answer with no preamble or commentary.\n\n")
	} else {
		sb.WriteString("An independent reviewer evaluated your previous answer and it must be improved before it can be returned. " +
			"Address the reviewer's feedback below: use your tools to fix the gaps — re-fetch and verify sources, correct or remove unsupported claims, add missing citations. " +
			"If you're unsure how to address this feedback, consult your advisor (ask_advisor) before revising — it knows this task's goal and can help you tell what actually needs to change. " +
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
	sb.WriteString(boundExcerpt(questionText(question), maxOriginalQuestionChars))
	sb.WriteString("\n\nYour previous answer:\n")
	sb.WriteString(boundExcerpt(answer, maxPreviousAnswerChars))
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
		sb.WriteString(boundExcerpt(section, maxActivitySectionChars))
		sb.WriteString("\n")
	}
	sb.WriteString("Question:\n")
	sb.WriteString(boundExcerpt(questionText(question), maxOriginalQuestionChars))
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sb.String()}}}
}

// continuationMarker opens every continuation prompt. It is the worker's signal
// that this turn CONTINUES an unfinished task (do the work) rather than starting
// or revising one — and the tests' handle on "the continuation actually landed".
const continuationMarker = "CONTINUE THE TASK — it is not finished."

// buildContinuationPrompt is the tool-bearing continuation directive for a worker
// whose work isn't done (workIncomplete): an empty turn, or a demanded commit/push
// the ledger doesn't show. It is deliberately NOT buildFinalizeContent — that one
// asks for a WRITE-UP, which is exactly how half-finished work got frozen in place
// and passed off to the judge. This one says: do the remaining work, with your
// tools, and name the known gap.
func buildContinuationPrompt(task string, act workerActivity, checks []string) string {
	var sb strings.Builder
	sb.WriteString(continuationMarker + "\n\n" +
		"Your last turn produced no answer, or produced an answer for work you have not actually delivered. " +
		"You are MID-TASK, not done. This is not a request for a summary.\n\n" +
		"Continue now, using your tools:\n" +
		"- DO the remaining work rather than describing it. Writing a file's contents into your answer is NOT writing the file — call write_file/edit_file.\n" +
		"- Run the checks the task requires and fix whatever they surface.\n" +
		"- When the task calls for it, commit your work, push the branch, and open the pull request.\n" +
		"- When the task calls for a posted review, record your findings with github_add_review_comment and submit them with github_submit_review. Writing findings into your answer is NOT posting a review.\n" +
		"- When the task calls for a review of a code change, EXECUTE the change with run_command before you judge it — run its tests, and write a throwaway harness that drives the code and prints what it does. Reading is not verification.\n" +
		"- Only once the work is actually done, report what you DID — past tense, evidenced by the tool calls you made.\n\n")
	if len(checks) > 0 {
		sb.WriteString("Checks this node must pass: " + strings.Join(checks, ", ") + "\n\n")
	}
	var gaps []string
	for _, c := range incompleteCriteria(task, act) {
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

// normalizeScale converts a verdict's scores from the rubric's 0–10 integer
// scale to the internal 0.0–1.0 axis the gate, deterministic criteria, and
// threshold all use. The rubric asks the judge for whole numbers 0–10 (an LLM
// scores more reliably on a coarse integer scale than on fine decimals, per
// G-Eval practice), but everything downstream works in 0–1.
//
// The scale is DETECTED rather than always divided: if no score exceeds 1.0 the
// judge answered in 0–1 (some models ignore the 0–10 instruction), so we leave
// it untouched. The one ambiguous case — a genuine 0–10 verdict whose every
// score is ≤1 (a uniformly catastrophic answer) — fails the gate under either
// reading, so the ambiguity is harmless. Idempotent: a second call is a no-op.
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

// aggregateVerdict derives the overall score from the per-criterion values: it
// takes the LOWEST criterion (weakest-link gating) and clamps to [0,1]. There is
// no averaging and no hard caps — a single failing criterion sinks the answer on
// its own. Scores must already be on the 0–1 axis (see normalizeScale). Used for
// both the submit_verdict path and the parseVerdict text fallback, and called
// again by the gate after it folds in the deterministic criteria; it is
// idempotent on the lowest value.
func aggregateVerdict(v verdict) verdict {
	// Per-criterion gating (DeepEval-style multi-metric composition): each
	// criterion is an independent requirement, so the overall score is the WEAKEST
	// criterion — the binding constraint. The gate passes only when every criterion
	// clears the threshold, so one fatal flaw (leaked preamble, no citations) can't
	// be averaged away by strong scores elsewhere. No hard caps: a low criterion
	// fails on its own and drives a targeted revision rather than code overriding.
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
// Scores are normalised from the rubric's 0–10 scale to 0–1 (normalizeScale),
// then the overall score is set to the LOWEST criterion (weakest-link gating, no
// averaging and no caps) and clamped to [0,1] by aggregateVerdict.
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

	normalizeScale(&v)
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

// runWriterFresh recovers an empty worker draft: it runs a TOOL-LESS writer (the
// worker's model, no tools) in a FRESH in-memory runner with the self-contained
// finalize prompt, which already carries the worker's findings. The fresh runner is
// the whole point — re-invoking the worker in its own session drops the finalize
// prompt (the llmagent rebuilds its request from session events, which end in the
// empty reply), so the write-up never happens. A tool-less writer composes the
// answer directly from the findings instead of re-researching.
func runWriterFresh(ctx context.Context, m model.LLM, content *genai.Content) (string, error) {
	if m == nil {
		return "", fmt.Errorf("vetting: no writer model for empty-answer recovery")
	}
	writer, err := llmagent.New(llmagent.Config{
		Name:        "finalize-writer",
		Description: "Composes the final answer from the findings provided, without tools.",
		Model:       m,
		Instruction: "You are a writer. Using ONLY the findings and instructions in the message, write the complete final answer now. You have no tools — do not attempt to call any; compose directly from what you are given.",
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
