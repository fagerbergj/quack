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

// Config carries per-agent trust-gate settings for RunGatedRefine and the judge.
type Config struct {
	// NodeBaseSHA is the clone's HEAD when THIS node started. Chained nodes share
	// one clone, so diffing from the reflog's oldest entry shows every sibling's
	// work too and the change-shape criteria fail on commits this node never made
	// (#710). Empty ⇒ fall back to the reflog base (single-node plans, no clone).
	NodeBaseSHA         string
	DeterministicRounds int             // cheap citation/length check + revise cycles
	JudgeRounds         int             // model-judge/revise rounds
	Threshold           float64         // pass score in (0,1]
	JudgeMaxIterations  int             // cap on judge model turns per round
	JudgeContextWindow  int             // context window in tokens; 0 ⇒ default
	Constitution        string          // global principles for judge prompt
	Rubric              string          // scoring guide; global default or per-agent override
	RequireRetrieval    bool            // zero retrieval = ungrounded
	ReadOnly            bool            // no delivery tools - completion is review/exploration
	IsReviewer          bool            // stamped from node agent, never from task wording
	Memory              *memory.Store   // staged tradecraft on pass
	CommitMemory        bool            // task-memory participant
	MemoryRole          string          // role bucket key; empty falls back to repo then user
	DeliverPromptEvent  bool            // true for A2A workers (session events)
	Checks              []string        // per-node deterministic gate commands
	DeriveChecks        bool            // derive from repo when Checks empty
	CheckCommands       []string        // prefix allowlist; empty ⇒ checks disabled
	NodeID              string          // workspace scope for checks/clone resolution
	AdvisorToken        string          // fs tool scope token; empty = no scope
	Agent               string          // observability only
	Task                string          // delivery check; empty = no check
	Workdir             string          // for Checks; ignored when Checks empty
	ChatID              string          // per-chat workspace scope
	Workspace           *workspace.Jail // nil + non-empty Checks fails closed
	WorkspaceUserID     string
	WorkspaceCaps       workspace.Caps
	Deliver             DeliverFunc    // posts staged delivery set; nil = disabled
	Grant               *Grant         // permission set; nil = unrestricted
	ExternalWorker      bool           // ACP-backed; gate supplements session ledger
	Setup               *SetupBranch   // pre-cloned checkout; delivery on this branch
	ExistingPR          bool           // run pushes onto an already-open PR; stage_push offered instead of stage_pr
	Skeptic             SkepticFactory // adversarial verify; nil = disabled
	SkepticRounds       int            // skeptics per finding; <= 0 = disabled
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

type fetchRecord struct {
	sample string
}

// workerActivity: worker's retrieval and workspace operations from session events.
type workerActivity struct {
	searches  []string
	fetched   map[string]fetchRecord
	seen      map[string]string
	staged    []memory.Candidate
	workspace []wsOp

	clonedRepos []string
	clonedDirs  []string
	paths       map[string]bool // successful fs ops paths, normalizePath'd

	written []string // jail-relative paths for buildChangedFilesSection

	committed bool
	pushed    bool
	prOpened  bool

	reviewCommented bool
	reviewSubmitted bool

	greps int // ACP grep/glob calls

	ranCommand bool

	stagedDelivery map[string]StagedDelivery
	currentBranch  string
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
