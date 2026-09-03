package vetting

import (
	"context"
	"iter"
	"sort"
	"strings"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/workspace"
)

// judgeSessionID: real chat id groups a judge/writer run under the chat that
// caused it in Langfuse (ADK stamps gen_ai.conversation.id from this session id).
// Each caller gets its own throwaway InMemoryService per run, so reusing chatID
// across calls can't leak conversation state between them (only observability
// grouping is affected). Empty chatID falls back rather than emitting "".
func judgeSessionID(chatID, fallback string) string {
	if chatID == "" {
		return fallback
	}
	return chatID
}

// Config carries per-agent trust-gate settings for RunGatedRefine and the judge.
type Config struct {
	// NodeBaseSHA is the clone's HEAD when THIS node started. Chained nodes share
	// one clone, so diffing from the reflog's oldest entry shows every sibling's
	// work too and the change-shape criteria fail on commits this node never made
	// (#710). Empty ⇒ fall back to the reflog base (single-node plans, no clone).
	NodeBaseSHA          string
	DeterministicRounds  int     // cheap citation/length check + revise cycles
	JudgeRounds          int     // model-judge/revise rounds
	Threshold            float64 // pass score in (0,1]
	JudgeMaxIterations   int     // cap on judge model turns per round
	JudgeContextWindow   int     // context window in tokens; 0 ⇒ default
	JudgeMaxOutputTokens int     // cap on judge/plan-judge reply tokens; <= 0 = uncapped
	Constitution         string  // global principles for judge prompt
	Rubric               string  // scoring guide; global default or per-agent override; rendered markdown for the judge prompt
	// RubricSpecs: per-criterion definition/scale/bands, only when Rubric was
	// loaded from a rubric.yaml (rubricyaml.go) - nil for a raw prose rubric
	// override (dag planner / inline GatesConfig.Rubric), which has no
	// structured criteria to look up (#941).
	RubricSpecs map[string]criterionSpec
	// RubricFixes: declared fix text per deterministic criterion the rubric
	// names (rubricyaml.go's rubricDocFixes) - nil for a raw prose override.
	RubricFixes      map[string]string
	RequireRetrieval bool // zero retrieval = ungrounded
	ReadOnly         bool // no delivery tools - completion is review/exploration
	IsReviewer       bool // stamped from node agent, never from task wording
	// ReviewFanout: non-nil only for a reviewer node in a plan with >1
	// reviewer node (#867). Such a node never delivers its own review -
	// it stages into ReviewFanout, and the last reviewer node to finish
	// delivers the merged, worst-of-verdict review exactly once.
	ReviewFanout *ReviewFanout
	Artifacts    artifact.Service // nil = read_artifact tool unavailable to this node
	// Ledger: the WAL's fail-closed AppendIntent path (#1090 §4.9/#1100). Wired
	// ONLY when observability.recording is enabled AND its store resolves to
	// kind "postgres" - the filesystem ledger's AppendIntent is best-effort,
	// non-transactional (see FSStore.AppendIntent), so it cannot back a
	// fail-closed write and is never set here. nil = no WAL, recordstore and
	// the gate behave exactly as before #1100.
	Ledger ledger.LedgerStore
	// Artifact: episodic record name this node writes on gate pass ("body" or
	// "" for none). "review" is written for IsReviewer nodes regardless of
	// this field - it names only the reMarkable-style extra record (#1006).
	Artifact           string
	Memory             *memory.Store   // staged tradecraft on pass
	CommitMemory       bool            // task-memory participant
	MemoryRole         string          // role bucket key; empty falls back to repo then user
	DeliverPromptEvent bool            // true for A2A workers (session events)
	Checks             []string        // per-node deterministic gate commands
	DeriveChecks       bool            // derive from repo when Checks empty
	CheckCommands      []string        // prefix allowlist; empty ⇒ checks disabled
	CheckSetup         []string        // repo bootstrap commands; run once per clone (checks.go, baseline.go) before checks are derived/run, both in the worker's tree and the base baseline worktree
	NodeID             string          // workspace scope for checks/clone resolution
	AdvisorToken       string          // fs tool scope token; empty = no scope
	Agent              string          // observability only
	User               string          // observability only; resolved from the ADK session, not caller-set
	Source             string          // observability only; run origin (extension name or a fixed app value)
	Task               string          // delivery check; empty = no check
	Workdir            string          // for Checks; ignored when Checks empty
	ChatID             string          // per-chat workspace scope
	Workspace          *workspace.Jail // nil + non-empty Checks fails closed
	WorkspaceUserID    string
	WorkspaceCaps      workspace.Caps
	Deliver            DeliverFunc         // posts staged delivery set; nil = disabled
	GitCredentials     GitCredentialSource // resolves the gate-owned push credential; nil = push disabled
	// AllowedDeliveryKinds: nil = unrestricted (no trigger governs this run);
	// non-nil (including empty) restricts staged delivery to exactly these kinds.
	AllowedDeliveryKinds []string
	ExternalWorker       bool         // ACP-backed; gate supplements session ledger
	Setup                *SetupBranch // pre-cloned checkout; delivery on this branch
	ExistingPR           bool         // run pushes onto an already-open PR; stage_push offered instead of stage_pr
	// JudgeModel: the raw model behind judge (set once at startup, same
	// instance NewJudgeFactory closes over) - stamped with per-round coords
	// the same way workerModel is, so its metrics don't rely on ctx alone.
	JudgeModel model.LLM
}

// SetupBranch mirrors dag.Plan.Setup delivery fields.
type SetupBranch struct {
	Repo       string
	WorkBranch string
}

// One item the worker staged for delivery. Branch filled by the gate.
type StagedDelivery struct {
	Kind   string // pull_request | review | comment
	Branch string
	Title  string
	Body   string
	// TitleOmitted/BodyOmitted: Kind pull_request via stage_push only - the agent
	// didn't supply that field, so delivery must PATCH without the key rather
	// than send an empty string (which would blank it on GitHub). Zero value
	// (false) matches every other path, which always carries both fields.
	TitleOmitted bool
	BodyOmitted  bool
	Event        string          // review verdict: approve | request_changes | comment
	Slot         string          // comment target, for Kind == "comment"
	Comments     []ReviewComment // inline findings
	Recovered    bool            // parsed from answer tail, not tool-staged
}

// ReviewComment: one inline, line-anchored review finding.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// DeliveryContext: staged set + clone coordinates for extension delivery.
type DeliveryContext struct {
	NodeID       string
	ChatID       string // for delivery-outcome bookkeeping
	Items        []StagedDelivery
	CloneURL     string // git_clone URL; "" = nothing to deliver
	CloneDir     string // jail-resolved clone path; "" = no Workspace
	Branch       string // last branch from ledger
	IssueNumber  int    // PR number for review/comment targets
	GatePassed   bool   // false = attach caveat
	GateFeedback string // feedback for caveat when GatePassed is false
	// PushedSHA: the branch head the gate itself pushed; "" = no push happened.
	PushedSHA string
	// ChecksSkipNote: non-empty when GatePassed but no build/test check ran
	// for a reason worth telling the reader (#780). Already worded for
	// display; "" means say nothing (checks ran, or the reason is operator
	// config, not a property of the change).
	ChecksSkipNote string
	// IdempotencyKey: target artifact id + revision (#1093 V4 §4.9) - "" when
	// this delivery has no backing artifact revision to key on.
	IdempotencyKey string
}

// DeliverFunc: posts final staged delivery. Errors logged, never fail the node.
type DeliverFunc func(ctx context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error)

// DeliveryItemOutcome: one staged item's actual delivery result.
type DeliveryItemOutcome struct {
	Kind  string
	URL   string
	Error string
}

// PromptEventNeeded: true for A2A agents, false for llmagent.
func PromptEventNeeded(ag adkagent.Agent) bool {
	type nodeRunner interface {
		RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error]
	}
	_, ok := ag.(nodeRunner)
	return !ok
}

// maxContinueRounds: tool-bearing continuation rounds before tool-less fallback.
const maxContinueRounds = 4

const fetchSampleBytes = 300 // bytes of fetched content per URL for judge spot-checking

// workerActivity: worker's retrieval and workspace operations from session events.
type workerActivity struct {
	searches  []string
	fetched   map[string]struct{}
	seen      map[string]string
	staged    []memory.Candidate
	workspace []wsOp

	clonedRepos []string
	clonedDirs  []string
	paths       map[string]bool // successful fs ops paths, normalizePath'd

	written []string // jail-relative paths for buildChangedFilesSection

	committed bool
	pushed    bool

	reviewCommented bool
	reviewSubmitted bool

	ranCommand bool

	answer string // the node's final answer (set just before commitDelivery; the synthesizer's is its review body, #965)

	stagedDelivery map[string]StagedDelivery
	currentBranch  string

	// skipArtifactRender: post stagedDelivery text as-is, never the
	// code_review/pr_body artifact - it may be stale relative to this
	// item (aborted round's salvaged text, or an already-merged review).
	skipArtifactRender bool
	// ponytail: prefer plan.Setup's PR/issue number over ledger inference.
	prNumber int
}

// wsOp: one completed fs/git/run_command call/response pair.
type wsOp struct {
	tool   string
	detail string
	sample string
}

// contentPlainText: concatenates plain-text parts of genai.Content.
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

// recordSearchResults: extracts {url: snippet} from web_search response. First snippet wins.
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

// sortedStagedDelivery: returns staged set sorted by target key for stable delivery order.
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

// trimToSample: truncates to fetchSampleBytes at valid UTF-8 boundary.
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
