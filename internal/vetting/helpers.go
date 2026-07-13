package vetting

import (
	"iter"
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
	Constitution        string  // global principles; prefixed in the judge prompt
	Rubric              string  // scoring guide; global default or per-agent override

	// RequireRetrieval marks an agent whose job is retrieval (its tool list
	// includes web_search/web_fetch). For such an agent an answer produced with
	// ZERO retrieval activity is deterministically ungrounded — either pure model
	// memory or (live e2e 2026-07-05) a question to the user written as answer
	// text instead of an ask_user call. The deterministic fold hard-fails it with
	// feedback naming both ways out (research it, or ask_user). False for
	// tool-less agents like the synthesizer, which legitimately re-cite upstream
	// URLs without retrieving anything themselves.
	RequireRetrieval bool

	// Memory, when set, receives the agent's staged tradecraft on a judge pass
	// (M6). nil disables the gated commit path.
	// ponytail: the gated-commit-on-pass path is not yet wired into RunGatedRefine
	// (dropped with the custom-agent gate); re-add in a memory follow-up.
	Memory *memory.Store
	// CommitMemory marks this agent as a task-memory participant.
	CommitMemory bool

	// DeliverPromptEvent makes the gate write each worker prompt into the
	// session as a gate-authored event right before the worker run (see
	// emitPrompt). Set via PromptEventNeeded: true for REMOTE (A2A) workers,
	// which build their outbound message from session events only and would
	// otherwise never see their prompt; false for local llmagents, which take
	// the RunNode input natively — for them the extra user-role event would be
	// worse than redundant: a concurrent node's prompt event shifts a
	// single-turn llmagent's "current turn" anchor, leaking one node's prompt
	// into another's request (caught by
	// TestAskAdvisor_ConcurrentNodesIsolatedThreads).
	DeliverPromptEvent bool

	// Checks are per-node, orchestrator-set deterministic gate commands (§4 of
	// .quack/plan-pr5-tool-schemas.md) — stamped onto a PER-NODE copy of this
	// Config by dag.buildGateNodes from the plan node's own Checks (already
	// plan-time validated: every entry prefix-matches a configured
	// workspace.check_commands entry and contains no shell metacharacters).
	// Empty for every node that doesn't opt in (research, synthesis) and for
	// every agent's BASE Config (see FromConfig) — this field only ever gets a
	// value per-node, never per-agent.
	Checks []string
	// DeriveChecks marks a node whose checks the gate DERIVES from the repo on
	// disk when Checks is empty (deriveChecks in checks.go) — set per-node by
	// dag.buildGateNodes for code-implementer nodes. The planner authors the DAG
	// before anything has looked at the repo, so it cannot know the repo's check
	// commands; a set Checks list is an explicit override that still wins.
	DeriveChecks bool
	// CheckCommands is the configured workspace.check_commands allowlist — the
	// security boundary every check must prefix-match, whether the planner wrote
	// it (dag.validateChecks) or the gate derived it. Empty ⇒ checks disabled.
	CheckCommands []string
	// NodeID identifies the node in the gate's check logs.
	NodeID string
	// Workdir is the workspace-relative directory Checks run in (the node's
	// repo, e.g. "repo" after a git_clone). When unset and checks are derived,
	// the gate locates the single repo in the node's workspace scope.
	Workdir string
	// ChatID is the per-chat workspace scope (the chat/session id) Checks
	// resolve their Workdir under, so a node's deterministic checks run in the
	// SAME <root>/<user>/<chatID>/<workdir> dir the node's own git_clone/fs tools
	// wrote to (see checksPassCriterion). Stamped per-node by dag.buildGateNodes
	// from the run's chat id; "" falls back to the per-user root (Jail.Resolve).
	ChatID string
	// Workspace/WorkspaceUserID/WorkspaceCaps are the SAME jail, identity, and
	// caps the fs/git/run_command tools use (internal/workspace) — wired once
	// onto the base Config in internal/serve's buildAgents, so a node's Checks
	// execute through the identical isolation boundary its own tool calls did
	// (see checksPassCriterion in checks.go). nil Workspace with non-empty
	// Checks fails closed rather than running unjailed.
	Workspace       *workspace.Jail
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps
}

// PromptEventNeeded reports whether worker prompts must be delivered as
// session events for this agent (Config.DeliverPromptEvent): true unless the
// agent implements the node-runner interface AgentNode feeds RunNode input
// through (only llmagent does; remote A2A agents don't — they build their
// message from session events instead. See emitPrompt).
func PromptEventNeeded(ag adkagent.Agent) bool {
	type nodeRunner interface {
		RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error]
	}
	_, ok := ag.(nodeRunner)
	return !ok
}

// maxEmptyRetries bounds the empty-answer recovery re-invocations in RunGatedRefine.
const maxEmptyRetries = 4

// fetchSampleBytes is how many bytes of fetched content we keep per URL — enough
// for the judge to spot-check a claim, small enough not to flood its context.
const fetchSampleBytes = 300

// fetchRecord is the retained sample of a fetched page, for judge spot-checking.
type fetchRecord struct {
	sample string
}

// workerActivity summarises the worker's retrieval AND workspace operations
// (reconstructed from session events by activityFromSession). Passed to the
// judge + deterministic citation check so neither can falsely claim no
// retrieval happened — and, via the workspace ledger, so the judge can check
// the answer's CLAIMS against what the worker actually did (live e2e
// 2026-07-10: a coder claimed "Committing…" + quoted README lines; ground
// truth had zero commits and no such README content — both invisible to a
// judge whose activity context recorded only web_search/web_fetch).
type workerActivity struct {
	searches  []string               // every web_search query
	fetched   map[string]fetchRecord // URL → sample for web_fetch calls that returned content
	seen      map[string]string      // URL → search snippet for surfaced-but-not-fetched URLs
	staged    []memory.Candidate     // memory candidates staged via stage_memory (M6)
	workspace []wsOp                 // fs/git/run_command operations, in session order (see ledger.go)

	// Cloned-repo grounding (live failure 2026-07-12: an explore-repo node
	// cloned a repo, read ~10 files, cited them — and cites_sources scored
	// 0.25, sinking a node the judge called excellent). A successful git_clone
	// puts the ENTIRE repo on local disk: every file in it is retrieved
	// material by construction, so citations of URLs under the repo and of
	// local paths inside the clone dir are real grounding, not fabrication.
	clonedRepos []string        // successful git_clone URLs
	clonedDirs  []string        // the local dirs those clones landed in (normalizePath'd)
	paths       map[string]bool // paths of successful fs ops (read/write/edit/delete), normalizePath'd

	// written are the jail-relative paths the worker actually created/modified
	// (successful write_file/edit_file only — not reads, not deletes), in
	// first-touch order, resolved against the cwd in effect at the time of the
	// call. buildChangedFilesSection re-reads these off disk so the judge scores
	// the REAL post-edit source, not the worker's self-report (live 2026-07-12: a
	// blind judge passed an incomplete, non-compiling deliverable it never read).
	written []string
}

// wsOp is one workspace operation the worker actually performed — a completed
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
// genuinely-retrieved lead — a valid source if later cited. First snippet wins.
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
