package vetting

import (
	crand "crypto/rand"
	"encoding/hex"
	"regexp"
	"sync"

	"github.com/fagerbergj/quack/internal/memory"
)

// Advisor-thread identity: how a worker's ask_advisor tool knows WHICH node's
// mentor conversation to continue.
//
// The tool's handler cannot derive the calling node from its own execution
// context: production workers are served over A2A (internal/agent.Serve →
// srv.Client()), so the handler runs inside the A2A SERVER's runner - a
// separate session (the A2A context session, AppName = the agent's name, a
// FRESH context per gate round) whose events carry no NodeInfo and whose
// state has none of the gate's keys. Branch is no help either: inside a tool
// call it is the worker's own run segment co-located ("worker@worker-r0", no
// node) and empty on the A2A server. The one channel that reliably crosses
// the A2A hop is the PROMPT - so the gate stamps a short marker line carrying
// a per-node token into every worker prompt, and the tool parses it back out
// of its UserContent. Deterministic and race-free: UserContent is fixed
// before the model ever runs.
//
// The token doubles as the key into a process-local registry carrying the
// node's task + acceptance rubric, which the tool uses to seed the mentor's
// session on its first consult. Process-local is sound here for the same
// reason the tool can hold the advisor agent at all: quack's A2A agents are
// co-located in one process (see internal/agent's package doc). If agents are
// ever promoted to standalone services, ask_advisor needs a rethink wholesale.

// advisorMarkerRe extracts the token from a marker line anywhere in the
// prompt. Plan/node IDs are slugs; anything up to the closing "]]" is the
// token.
var advisorMarkerRe = regexp.MustCompile(`\[\[quack:advisor-thread:([^\]]+)\]\]`)

// AdvisorThreadToken derives the stable per-node token: same across gate
// rounds (draft → revision), steered re-runs, and HITL pause/resume (all
// re-derive from the same plan+node), distinct across concurrent nodes and
// across plans.
func AdvisorThreadToken(planID, nodeID string) string {
	return planID + "/" + nodeID
}

// AdvisorThreadMarker renders the marker line the gate APPENDS to a worker
// prompt. Models treat the bracketed line as inert metadata. Trailing (not
// leading) placement pairs with ParseAdvisorThread's last-match rule: text
// that can carry a FOREIGN node's marker - an upstream output quoted into
// this prompt, or (over A2A) an earlier concurrent node's prompt event swept
// into this dispatch's message tail - always precedes this node's own
// trailing marker.
func AdvisorThreadMarker(token string) string {
	return "[[quack:advisor-thread:" + token + "]]"
}

// ParseAdvisorThread extracts the LAST advisor-thread token from prompt text
// (see AdvisorThreadMarker for why last); ok=false when no marker is present
// (e.g. the agent was invoked directly, outside any gated node).
func ParseAdvisorThread(text string) (token string, ok bool) {
	ms := advisorMarkerRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return "", false
	}
	return ms[len(ms)-1][1], true
}

// AdvisorTask is what the mentor is told on first consult - the node's task
// and its acceptance rubric (the desired outcome) - plus the WORKFLOW session
// coordinates the guard ladder needs (internal/tools/guard.go): over the A2A
// hop a guarded tool executes inside the A2A SERVER's runner, whose own
// ctx.AppName()/UserID()/SessionID()/InvocationID() name the A2A context
// session - a fresh, per-round session that holds NONE of the confirm
// pause/resume events (adk_request_confirmation calls, the human's resume
// FunctionResponse, GuardResolvedKey consumption markers all live in the
// workflow/chat session). The gate registers these coordinates here (it runs
// workflow-side and knows them), and the guard looks them up by the same
// thread token to scan the RIGHT session. Registered identically for
// co-located workers, so there is exactly one lookup path.
type AdvisorTask struct {
	Task   string
	Rubric string
	// NodeID is the plan node's REAL identity - the key node-level bookkeeping
	// (cancel/steer controls, HITL interrupt IDs) is registered under, read
	// back by internal/tools' cancel/steer guards (cancelguard.go,
	// steerguard.go). Never redirect this to a shared workspace scope: doing
	// so would let cancelling one chain node accidentally match another.
	NodeID string
	// WorkspaceNodeID is the workspace-relative "node" a call's fs/git tools
	// default their directory scope to (internal/tools scopeFromContext →
	// workspace.NodeDir) - node.ID for almost every node, but shared across a
	// plan.Setup chain's repo-touching nodes (see dag.workspaceNodeID). Empty
	// falls back to NodeID (every caller but dag's chain-aware one leaves it
	// unset).
	WorkspaceNodeID string
	// WorktreeParent is the WorkspaceNodeID of the plan's shared setup clone
	// (workspace.SharedRepoScope) when THIS node's own WorkspaceNodeID is a
	// git worktree linked off it, rather than an independent directory - set
	// only for a read-only qualifying node (reviewer/explorer) in a
	// plan.Setup chain (see dag.worktreeParentID). Empty for every other node:
	// the writer sharing the clone directly, and any node with no Setup at
	// all. internal/acp's resolveNode reads this to provision the worktree
	// (via Options.Worktree) before handing the round its cwd.
	WorktreeParent string
	// Workflow session coordinates + invocation, for guard-ladder scans.
	AppName      string
	UserID       string
	SessionID    string
	InvocationID string

	// MemSecret (#344) is this node's credential for the ACP memory MCP surface
	// (internal/acp) - an unguessable value MINTED FRESH per node (NewMemSecret),
	// never derived from plan/node IDs. The advisor-thread token above is a poor
	// substitute: it's pure planID+nodeID concatenation, and a worker's own prompt
	// discloses its running siblings' node IDs (executor.go siblingIDs) - an
	// untrusted external subprocess could reconstruct another node's token and
	// reach its memory bucket. MemSecret is looked up in a SEPARATE registry
	// (memSessions, keyed by the secret itself, not the token) precisely so the
	// two can never be confused. Empty when the node isn't a memory participant.
	MemSecret string
}

// MemSession is what the ACP memory MCP surface resolves for ONE node's
// load_memory/stage_memory calls - registered under MemSecret (never the
// advisor-thread token; see AdvisorTask.MemSecret) so a guessed/derived token
// buys an attacker nothing here.
type MemSession struct {
	Memory *memory.Store
	Scope  memory.Scope
	// Staged is the round's stage_memory landing buffer. The gate drains it
	// (MemStage.Drain) into the worker's activity right before commitMemoryOnPass,
	// so an MCP-staged candidate commits through the exact same pass-gated path
	// as a native agent's stage_memory tool call.
	Staged *MemStage
	// Review is the review MCP surface's landing buffer (stage_review_comment +
	// stage_review, internal/acp reviewmcp.go). Non-nil ONLY on a review-delivery
	// node (an external read-only reviewer whose task demands a posted review) -
	// its presence is what registers the two review tools. The gate reads a
	// Snapshot into act.stagedDelivery["review"] so a tool-staged review beats
	// the answer-tail fallback (augmentFromAnswer). nil ⇒ the tools aren't offered.
	Review *ReviewStage
	// PRStage is the stage_pr MCP surface's landing buffer (internal/acp
	// reviewmcp.go). Non-nil ONLY on an implement-delivery node (an external
	// WRITE worker at the chain's terminal delivery point) - its presence is what
	// registers stage_pr. The gate snapshots it into act.stagedDelivery["pr"] so a
	// skill-authored title+body beats augmentFromRepo's commit-subject fallback.
	PRStage *PRStage
}

// MemStage is a per-node, mutex-guarded staging buffer stage_memory (over the
// ACP memory MCP surface) appends candidates to across every round of a node.
type MemStage struct {
	mu    sync.Mutex
	items []memory.Candidate
}

// ReviewStage is a per-node, mutex-guarded staging buffer the review MCP surface
// (internal/acp reviewmcp.go) fills across a review node's rounds: an inline
// comment per stage_review_comment call and the overall verdict+summary from
// stage_review. Unlike MemStage it is READ, not drained - the gate snapshots it
// on every round (Snapshot) and the same snapshot survives into commitDelivery.
type ReviewStage struct {
	mu       sync.Mutex
	event    string
	body     string
	set      bool // a stage_review (verdict) call landed
	comments []ReviewComment
}

// AddComment stages one inline, line-anchored finding.
func (s *ReviewStage) AddComment(path string, line int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments = append(s.comments, ReviewComment{Path: path, Line: line, Body: body})
}

// SetVerdict records the review's overall event + summary; a later call replaces
// an earlier one (the reviewer's final word wins).
func (s *ReviewStage) SetVerdict(event, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event, s.body, s.set = event, body, true
}

// Snapshot renders the staged review as a StagedDelivery WITHOUT clearing the
// buffer (the gate reads it every round). ok is false until at least one comment
// or a verdict is staged. Comments without an explicit verdict post as a plain
// comment review, mirroring augmentFromAnswer's verdict-less fallback.
func (s *ReviewStage) Snapshot() (StagedDelivery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set && len(s.comments) == 0 {
		return StagedDelivery{}, false
	}
	event := s.event
	if event == "" {
		event = "comment"
	}
	return StagedDelivery{
		Kind:     "review",
		Event:    event,
		Body:     s.body,
		Comments: append([]ReviewComment(nil), s.comments...),
	}, true
}

// PRStage is a per-node, mutex-guarded buffer the stage_pr MCP tool
// (internal/acp reviewmcp.go) fills on an implement-delivery node: the PR title
// and body the worker authored via the pr-authoring skill. Like ReviewStage it
// is READ, not drained - the gate snapshots it each round (Snapshot), and the
// same snapshot survives into commitDelivery.
type PRStage struct {
	mu    sync.Mutex
	title string
	body  string
	set   bool
}

// Set records the PR title + body; a later call replaces an earlier one.
func (s *PRStage) Set(title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title, s.body, s.set = title, body, true
}

// Snapshot renders the staged PR as a StagedDelivery (Kind pull_request - the
// delivery discriminator). The branch is left empty: the worker authored only
// text, so the gate fills the branch from the disk probe (augmentFromPRStage).
// ok is false until stage_pr lands.
func (s *PRStage) Snapshot() (StagedDelivery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return StagedDelivery{}, false
	}
	return StagedDelivery{Kind: "pull_request", Title: s.title, Body: s.body}, true
}

// Add appends one staged candidate.
func (s *MemStage) Add(c memory.Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, c)
}

// Drain returns everything staged so far and clears the buffer.
func (s *MemStage) Drain() []memory.Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.items
	s.items = nil
	return out
}

// NewMemSecret mints a fresh, unguessable per-node credential for the memory
// MCP surface - 256 bits from crypto/rand, hex-encoded. Deliberately
// independent of AdvisorThreadToken: that token is derivable (planID+nodeID)
// and disclosed to sibling nodes via the prompt, so it cannot double as a
// bearer credential handed to an untrusted external subprocess.
func NewMemSecret() (string, error) {
	var b [32]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// memSessions is the process-local secret → MemSession registry - SEPARATE
// from advisorThreads (keyed by the guessable advisor-thread token) precisely
// so the memory MCP surface can never be reached via a derived/guessed token.
var memSessions sync.Map

// RegisterMemSession publishes a node's memory session under its secret. A
// no-op for an empty secret (nothing to register, nothing reachable).
func RegisterMemSession(secret string, s MemSession) {
	if secret == "" {
		return
	}
	memSessions.Store(secret, s)
}

// LookupMemSession returns the registered session for secret, if any.
func LookupMemSession(secret string) (MemSession, bool) {
	if secret == "" {
		return MemSession{}, false
	}
	v, ok := memSessions.Load(secret)
	if !ok {
		return MemSession{}, false
	}
	s, ok := v.(MemSession)
	return s, ok
}

// UnregisterMemSession removes a secret's entry - called the moment the gate
// has drained its staging buffer (node.go) so a straggler MCP call arriving
// after the node's own commit decision fails outright instead of writing into
// a buffer nobody will ever drain again. A no-op for an empty/already-removed
// secret.
func UnregisterMemSession(secret string) {
	if secret == "" {
		return
	}
	memSessions.Delete(secret)
}

// advisorThreads is the process-local token → AdvisorTask registry. Written
// by the gated node (RegisterAdvisorThread before its worker runs, unregister
// when the node body exits - a HITL re-entry or retry re-registers), read by
// the ask_advisor tool on a thread's first consult.
var advisorThreads sync.Map

// RegisterAdvisorThread publishes a node's task+rubric under its token.
func RegisterAdvisorThread(token string, t AdvisorTask) {
	advisorThreads.Store(token, t)
}

// LookupAdvisorThread returns the registered task for token, if any.
func LookupAdvisorThread(token string) (AdvisorTask, bool) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return AdvisorTask{}, false
	}
	t, ok := v.(AdvisorTask)
	return t, ok
}

// UnregisterAdvisorThread removes a token's entry (bounds registry growth on
// a long-running server; the mentor's session itself persists regardless).
func UnregisterAdvisorThread(token string) {
	advisorThreads.Delete(token)
}
