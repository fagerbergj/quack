package vetting

import (
	"context"
	"iter"
	"sort"
	"strings"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Config carries the per-agent trust-gate settings consumed by RunGatedRefine
// (the native refine loop) and the independent judge (judge.go). Built from
// config via FromConfig (rubric.go), with a per-agent rubric override applied by
// the caller.
type Config struct {
	DeterministicRounds int     // cheap citation/length check + targeted revise cycles
	JudgeRounds         int     // expensive model-judge/revise rounds
	Threshold           float64 // judge pass score in (0,1]
	JudgeMaxIterations  int     // cap on the agentic judge's model turns per round (0 ⇒ default)
	// JudgeContextWindow is the judge model's context window in tokens
	// (config gates.judge.context_window), used to budget the assembled
	// judge prompt before the call: 0 ⇒ falls back to defaultJudgeContextWindow.
	JudgeContextWindow int
	Constitution       string // global principles; prefixed in the judge prompt
	Rubric             string // scoring guide; global default or per-agent override

	// RequireRetrieval marks an agent whose job is retrieval (its tool list
	// includes web_search/web_fetch). For such an agent an answer produced with
	// ZERO retrieval activity is deterministically ungrounded - either pure model
	// memory or a question to the user written as answer text instead of an
	// ask_user call. The deterministic fold hard-fails it with
	// feedback naming both ways out (research it, or ask_user). False for
	// tool-less agents like the synthesizer, which legitimately re-cite upstream
	// URLs without retrieving anything themselves.
	RequireRetrieval bool

	// ReadOnly marks an agent with no delivery tools (no git_commit/git_push) - a
	// code-reviewer or code-explorer. Such an agent can NEVER commit or push, so a
	// delivery demand read off its task (e.g. a review task polluted with the PR's
	// own "Add …/open a PR" description) is unsatisfiable and must not apply - its
	// completion is review_posted / exploration, never delivery.
	ReadOnly bool

	// IsReviewer marks a node whose AGENT is dag's reviewerAgent (code-reviewer),
	// stamped structurally by dag.buildGateNodes from node.AgentName - never from
	// the task's wording. A code-reviewer node is a review-delivery node by
	// construction: review-staging (minting stage_review/stage_review_comment),
	// review_posted/behaviour_verified completion, and the answer-tail fallback
	// (augmentFromAnswer) all key off this instead of a task-text regex, which
	// used to leave the whole review-delivery path disabled for a task with no
	// posting verb - e.g. the label-review default, "Review this pull request."
	// (#482). A ReadOnly node whose task merely MENTIONS review stays false here,
	// which is what keeps #471 (a spurious review staged off task text) fixed.
	IsReviewer bool

	// Memory, when set, receives the agent's staged tradecraft on a judge pass
	// (M6). nil disables the gated commit path.
	Memory *memory.Store
	// CommitMemory marks this agent as a task-memory participant.
	CommitMemory bool
	// MemoryRole is the agent's role family (memory.RoleCoding | memory.RoleResearch)
	// - the key of the role bucket it reads and writes. Memory is SHARED and bucketed
	// by subject, not siloed per agent: what the explorer learns about a repo, the
	// implementer and the reviewer recall (see internal/memory/scope.go). Empty = the
	// agent has no role bucket (its writes fall back to repo, then user).
	MemoryRole string

	// DeliverPromptEvent makes the gate write each worker prompt into the
	// session as a gate-authored event right before the worker run (see
	// emitPrompt). Set via PromptEventNeeded: true for REMOTE (A2A) workers,
	// which build their outbound message from session events only and would
	// otherwise never see their prompt; false for local llmagents, which take
	// the RunNode input natively - for them the extra user-role event would be
	// worse than redundant: a concurrent node's prompt event shifts a
	// single-turn llmagent's "current turn" anchor, leaking one node's prompt
	// into another's request (caught by
	// TestAskAdvisor_ConcurrentNodesIsolatedThreads).
	DeliverPromptEvent bool

	// Checks are per-node, orchestrator-set deterministic gate commands (§4 of
	// .quack/plan-pr5-tool-schemas.md) - stamped onto a PER-NODE copy of this
	// Config by dag.buildGateNodes from the plan node's own Checks (already
	// plan-time validated: every entry prefix-matches a configured
	// workspace.check_commands entry and contains no shell metacharacters).
	// Empty for every node that doesn't opt in (research, synthesis) and for
	// every agent's BASE Config (see FromConfig) - this field only ever gets a
	// value per-node, never per-agent.
	Checks []string
	// DeriveChecks marks a node whose checks the gate DERIVES from the repo on
	// disk when Checks is empty (deriveChecks in checks.go) - set per-node by
	// dag.buildGateNodes for code-implementer nodes. The planner authors the DAG
	// before anything has looked at the repo, so it cannot know the repo's check
	// commands; a set Checks list is an explicit override that still wins.
	DeriveChecks bool
	// CheckCommands is the configured workspace.check_commands allowlist - the
	// security boundary every check must prefix-match, whether the planner wrote
	// it (dag.validateChecks) or the gate derived it. Empty ⇒ checks disabled.
	CheckCommands []string
	// NodeID is the workspace-directory scope this node's checks/clone
	// resolution use (workspace.NodeDir(NodeID), workspace.SetupCloneDir(NodeID))
	// and its label in the gate's check logs - node.ID for almost every node,
	// but the ONE shared clone dir for a repo-touching node sharing a
	// plan.Setup chain (dag.buildGateNodes stamps it via dag.workspaceNodeID).
	NodeID string
	// AdvisorToken is this node's advisor-thread token (ParseAdvisorThread's
	// match on the draft prompt), set by RunGatedRefine right after it derives
	// it (see markerLine). The agentic judge runs in its OWN fresh session -
	// runJudgeRound stamps this into the content it hands the judge (mirroring
	// AdvisorThreadMarker's placement in a worker's continuation/revise prompt)
	// so the judge's read-only fs tools (scopeFromContext, internal/tools/cwd.go)
	// resolve into the SAME node scope the worker used, not the chat root above
	// it. Empty outside a gated node - the judge then holds no fs scope (no
	// clone to reach) and its tools fall back to the unscoped root.
	AdvisorToken string
	// Agent is the plan node's agent name (n.AgentName), stamped per-node by
	// dag.buildGateNodes - carried only for observability (span/metric
	// attribute), never branched on inside the gate itself.
	Agent string
	// Task is the node's task text, stamped per-node by dag.buildGateNodes. Read
	// by the deterministic delivery check (delivery.go): a task that demands the
	// work be committed/pushed/opened as a PR cannot pass without the ledger
	// showing it happened. Empty ⇒ the check simply doesn't apply.
	Task string
	// Workdir is the workspace-relative directory Checks run in (the node's
	// repo, e.g. "repo" after a git_clone). Ignored when Checks is empty (see
	// checksDir): when checks are DERIVED, the gate locates the single repo in
	// the node's workspace scope instead - a planner model doesn't always honor
	// "only set this alongside explicit checks" (#620).
	Workdir string
	// ChatID is the per-chat workspace scope (the chat/session id) Checks
	// resolve their Workdir under, so a node's deterministic checks run in the
	// SAME <root>/<user>/<chatID>/<workdir> dir the node's own git_clone/fs tools
	// wrote to (see checksPassCriterion). Stamped per-node by dag.buildGateNodes
	// from the run's chat id; "" falls back to the per-user root (Jail.Resolve).
	ChatID string
	// Workspace/WorkspaceUserID/WorkspaceCaps are the SAME jail, identity, and
	// caps the fs/git/run_command tools use (internal/workspace) - wired once
	// onto the base Config in internal/serve's buildAgents, so a node's Checks
	// execute through the identical isolation boundary its own tool calls did
	// (see checksPassCriterion in checks.go). nil Workspace with non-empty
	// Checks fails closed rather than running unjailed.
	Workspace       *workspace.Jail
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps

	// Deliver posts a node's judge-passed staged delivery set (M0.5's
	// staged-delivery spine - see StagedDelivery, commitDelivery). nil
	// disables delivery entirely: the staged set is simply dropped, exactly
	// like a nil Memory disables the memory-commit path.
	Deliver DeliverFunc

	// Grant is the trigger's computed permission set (#657, #662), stamped
	// per-node from dag.Plan.Grant. commitDelivery refuses any staged item
	// whose Kind the grant does not permit - the one enforcement point,
	// since delivery is the one place a run can actually mutate GitHub (ACP
	// workers can't git push; native write-side tools were deleted in
	// 0.6.0). nil means no GitHub trigger governs this run (unrestricted).
	Grant *Grant

	// ExternalWorker marks an ACP-backed agent (internal/acp): the worker has
	// none of quack's tools, so the gate supplements the session ledger with
	// ground truth it gathers itself - the git disk probe (augmentFromRepo) and
	// the answer-derived staged review (augmentFromAnswer).
	ExternalWorker bool

	// Setup is the plan's declared PRE-step (dag.Plan.Setup), stamped per-node
	// by dag.buildGateNodes for a repo-touching node (implementer/reviewer)
	// when the plan declared one. Non-nil means the harness already cloned
	// Repo and checked out WorkBranch before this node ran - commitDelivery
	// delivers on THAT branch, never the worker's own git-tracking ledger
	// (act.currentBranch), which a setup-provisioned worker is told not to touch.
	Setup *SetupBranch

	// Skeptic, when set, backs the adversarial verify stage (#370): after the
	// primary judge scores a round, SkepticRounds independent skeptics each
	// try to REFUTE its load-bearing PASSING criteria (adversarial.go). nil
	// disables the stage entirely, exactly like a nil judge disables judging.
	Skeptic SkepticFactory
	// SkepticRounds is N - how many independent skeptics adversarialVerify
	// spawns per load-bearing finding; a finding is killed only when a STRICT
	// MAJORITY refute it. <= 0 disables the stage.
	SkepticRounds int
}

// SetupBranch mirrors the delivery-relevant fields of dag.Plan.Setup (a small
// copy, not a dag import: internal/dag already imports internal/vetting, so
// the reverse import would cycle).
type SetupBranch struct {
	Repo       string
	WorkBranch string
}

// StagedDelivery is one item a worker staged for the gate to post on judge
// pass - a pull request, a review, or a comment. Branch is filled in by the
// gate from the ledger (the branch the worker last checked out/created), not
// by the worker: the worker never names a remote or a push target itself.
type StagedDelivery struct {
	Kind   string // pull_request | review | comment
	Branch string
	Title  string
	Body   string
	Event  string // review verdict: approve | request_changes | comment
	Slot   string // comment target, for Kind == "comment"
	// Comments are the review's inline findings (Kind == "review"), parsed from
	// an external reviewer's structured answer (augmentFromAnswer) - delivery
	// posts them as line-anchored review comments.
	Comments []ReviewComment
	// Recovered is true when this delivery was reconstructed from the answer's
	// tail-parse (augmentFromAnswer) rather than staged via the round's own
	// tools (#688) - reviewCriterion reads it so a recovered review is never
	// worded identically to a tool-staged one.
	Recovered bool
}

// ReviewComment is one inline, line-anchored review finding.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// DeliveryContext is what commitDelivery hands to Config.Deliver: the
// worker's FINAL staged set plus the on-disk/remote coordinates of the clone
// it committed to - everything an extension needs to push and post without
// re-deriving them from the session itself.
type DeliveryContext struct {
	NodeID string
	// ChatID is the run's chat/session id (cfg.ChatID) - an extension can key
	// its OWN delivery-outcome bookkeeping by it, since a boolean derived from
	// the SSE stream alone (e.g. "the judge passed") can't distinguish
	// "delivered" from "the gate passed but the post itself then failed".
	ChatID string
	Items  []StagedDelivery
	// CloneURL is the git_clone URL the worker cloned from ("" if none -
	// nothing to deliver against).
	CloneURL string
	// CloneDir is the ABSOLUTE, jail-resolved filesystem path of that clone
	// ("" if unresolvable - e.g. no Workspace configured).
	CloneDir string
	// Branch is the branch checked out/created in CloneDir when the worker
	// finished (from the ledger's last successful git_checkout/git_branch),
	// best-effort.
	Branch string
	// IssueNumber is the pull request a staged review/comment targets (from
	// the ledger's github_add_review_comment/github_submit_review calls). 0
	// when unavailable - a review/comment item then can't be delivered (logged,
	// not fatal to the rest of the set). A staged pull_request item ignores it
	// EXCEPT as the closing-issue target when the App backfills it from the
	// chat id (github's Deliver: only a fresh PR open acts on it, never an
	// update to an already-open PR).
	IssueNumber int
	// GatePassed is the node's final judge verdict. Delivery fires regardless
	// (graceful degradation), but a false here tells App.Deliver to attach a
	// caveat to the delivered PR/comment so a human reviews the gate's concerns.
	GatePassed bool
	// GateFeedback is the judge's actionable feedback (the lowest-criteria
	// notes), surfaced in the caveat when GatePassed is false. "" when it passed.
	GateFeedback string
}

// DeliverFunc posts a node's FINAL staged delivery set to the outside world -
// exactly once, when commitDelivery calls it (regardless of verdict - a caveat is attached on a fail). Errors
// are logged by the caller, never fail the node: delivery is best-effort like
// the memory-commit path, but unlike it, its failure is user-visible (the
// extension's own dispatch path posts a failure comment - see
// internal/github/webhook.go).
//
// The returned []DeliveryItemOutcome is what commitDelivery turns into
// per-item `delivery_result` stream events - the extension's own record of
// what each staged item actually produced (a real PR/review URL, or a
// per-item error), not a self-report. May be shorter than dc.Items (e.g. the
// branch push itself failed before any item was attempted) or empty (a
// pre-Items failure) - commitDelivery falls back to one event per dc.Item
// with the aggregate error when the extension reports nothing at all.
type DeliverFunc func(ctx context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error)

// DeliveryItemOutcome is one staged item's ACTUAL delivery result, as the
// extension observed it - never the worker's self-report.
type DeliveryItemOutcome struct {
	Kind  string // pull_request | review | comment (mirrors StagedDelivery.Kind)
	URL   string // best-effort; "" when the extension has nothing to report
	Error string // "" on success
}

// PromptEventNeeded reports whether worker prompts must be delivered as
// session events for this agent (Config.DeliverPromptEvent): true unless the
// agent implements the node-runner interface AgentNode feeds RunNode input
// through (only llmagent does; remote A2A agents don't - they build their
// message from session events instead. See emitPrompt).
func PromptEventNeeded(ag adkagent.Agent) bool {
	type nodeRunner interface {
		RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error]
	}
	_, ok := ag.(nodeRunner)
	return !ok
}

// maxContinueRounds bounds the tool-bearing continuation turns RunGatedRefine
// gives a worker whose WORK isn't finished (empty draft, or a demanded commit/push
// the ledger doesn't show) before falling back to the tool-less writer.
const maxContinueRounds = 4

// fetchSampleBytes is how many bytes of fetched content we keep per URL - enough
// for the judge to spot-check a claim, small enough not to flood its context.
const fetchSampleBytes = 300

// fetchRecord is the retained sample of a fetched page, for judge spot-checking.
type fetchRecord struct {
	sample string
}

// workerActivity summarises the worker's retrieval AND workspace operations
// (reconstructed from session events by activityFromSession). Passed to the
// judge + deterministic citation check so neither can falsely claim no
// retrieval happened - and, via the workspace ledger, so the judge can check
// the answer's CLAIMS against what the worker actually did (a coder once
// claimed commits that never happened).
type workerActivity struct {
	searches  []string               // every web_search query
	fetched   map[string]fetchRecord // URL → sample for web_fetch calls that returned content
	seen      map[string]string      // URL → search snippet for surfaced-but-not-fetched URLs
	staged    []memory.Candidate     // memory candidates staged via stage_memory (M6)
	workspace []wsOp                 // fs/git/run_command operations, in session order (see ledger.go)

	// Cloned-repo grounding: a successful git_clone puts the ENTIRE repo on
	// local disk - every file in it is retrieved material by construction, so
	// citations of URLs under the repo and of local paths inside the clone dir
	// are real grounding, not fabrication.
	clonedRepos []string        // successful git_clone URLs
	clonedDirs  []string        // the local dirs those clones landed in (normalizePath'd)
	paths       map[string]bool // paths of successful fs ops (read/write/edit/delete), normalizePath'd

	// written are the jail-relative paths the worker actually created/modified
	// (successful write_file/edit_file only - not reads, not deletes), in
	// first-touch order, resolved against the cwd in effect at the time of the
	// call. buildChangedFilesSection re-reads these off disk so the judge scores
	// the REAL post-edit source, not the worker's self-report.
	written []string

	// Delivery actions the worker actually completed (SUCCESSFUL calls only -
	// exactly like `written`): a git_commit, a git_push, and a github_pull_request.
	// Read by the deterministic delivery check (delivery.go).
	committed bool
	pushed    bool
	prOpened  bool

	// The reviewer's equivalent: a drafted inline comment
	// (github_add_review_comment) and a SUBMITTED review (github_submit_review).
	// Only the submit actually posts anything - the comments accumulate in a
	// process-local draft until then (see internal/github). Read by the
	// deterministic review check (delivery.go).
	reviewCommented bool
	reviewSubmitted bool

	// greps counts grep/glob-style search calls (ACP ToolKindSearch, mapped to
	// the tool name "search" - see internal/acp/translate.go) - an ACP agent's
	// directory-search activity, which the workspace ledger otherwise has no
	// slot for (it isn't a read_file/write_file/... op). Read by the
	// exploration_grounded check (node.go) alongside act.paths.
	greps int

	// ranCommand marks at least one SUCCESSFUL run_command - the worker EXECUTED
	// something (a test run, a build, a throwaway probe it wrote) rather than only
	// reading. Read by the deterministic behaviour check (delivery.go): a code
	// review produced purely by reading has verified nothing.
	ranCommand bool

	// stagedDelivery is the worker's CURRENT staged-delivery set (M0.5): a
	// keyed, MUTABLE map ("pr" / "review" / "comment:<slot>" → the latest
	// stage_pr/stage_review/stage_comment call for that target), so a later
	// call REPLACES an earlier one and unstage DROPS one - never an append
	// log. commitDelivery posts exactly this map's contents at the
	// moment the gate passes; anything unstaged or superseded before then
	// never reaches GitHub.
	stagedDelivery map[string]StagedDelivery
	// currentBranch is the branch the worker last successfully checked out or
	// created (git_checkout/git_branch), read by commitDelivery to know
	// what to push - the worker names it, but never pushes it itself.
	currentBranch string
	// prNumber is the pull request a review/comment target, read from the
	// first SUCCESSFUL github_add_review_comment or github_submit_review call
	// (whichever the worker made - reviewing an EXISTING PR always names its
	// number). 0 when the worker made neither call this session (an implement
	// task, or a review that found nothing to comment on).
	//
	// ponytail: once plan.Setup carries the triggering PR/issue number
	// explicitly, prefer that over this ledger inference - it is unavailable
	// for a clean-approval review that called neither tool.
	prNumber int
}

// wsOp is one workspace operation the worker actually performed - a completed
// call/response pair for an fs, git, or run_command tool. detail is a
// one-line summary (key args → key result fields, or "FAILED: …"); sample is
// read_file's head-of-content excerpt (fetchRecord-style), letting the judge
// spot-check quoted file content exactly as fetched-page samples let it
// spot-check web quotes.
type wsOp struct {
	tool   string
	detail string
	sample string
}

// contentPlainText concatenates the plain-text parts of a content.
func contentPlainText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// recordSearchResults extracts {url: snippet} pairs from a web_search response
// (shape {results: [{title, url, snippet}]}) into seen. Each surfaced URL is a
// genuinely-retrieved lead - a valid source if later cited. First snippet wins.
func recordSearchResults(seen map[string]string, resp map[string]any) {
	if resp == nil {
		return
	}
	var items []any
	switch r := resp["results"].(type) {
	case []any:
		items = r
	case []map[string]any:
		items = make([]any, len(r))
		for i, m := range r {
			items[i] = m
		}
	default:
		return
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		snippet, _ := m["snippet"].(string)
		seen[u] = strings.TrimSpace(trimToSample(snippet))
	}
}

// sortedStagedDelivery renders a worker's staged-delivery set as a slice in a
// STABLE order (sorted by target key) - commitDelivery's input, so
// delivery order never depends on map iteration.
func sortedStagedDelivery(staged map[string]StagedDelivery) []StagedDelivery {
	if len(staged) == 0 {
		return nil
	}
	keys := make([]string, 0, len(staged))
	for k := range staged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]StagedDelivery, 0, len(keys))
	for _, k := range keys {
		out = append(out, staged[k])
	}
	return out
}

// trimToSample truncates s to fetchSampleBytes at a valid UTF-8 boundary.
func trimToSample(s string) string {
	if len(s) <= fetchSampleBytes {
		return s
	}
	s = s[:fetchSampleBytes]
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}
