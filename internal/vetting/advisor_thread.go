package vetting

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

// advisorMarkerRe extracts the token from the marker line.
var advisorMarkerRe = regexp.MustCompile(`\[\[quack:advisor-thread:([^\]]+)\]\]`)

// AdvisorThreadToken: stable per-node token.
func AdvisorThreadToken(planID, nodeID string) string {
	return planID + "/" + nodeID
}

// AdvisorSessionApp/AdvisorSessionUser: the fixed ADK session identity every
// ask_advisor consult (internal/tools.NewAskAdvisorTool) is stored under -
// exported so a node's own cleanup (dag.newGatedNode) can delete the same row without duplicating this naming.
const (
	AdvisorSessionApp  = "quack-advisor"
	AdvisorSessionUser = "advisor"
)

// AdvisorSessionID returns the ADK session id an advisor thread's consults
// are stored under.
func AdvisorSessionID(token string) string { return token + ":advisor" }

// AdvisorThreadMarker: trailing marker (last-match rule handles foreign markers).
func AdvisorThreadMarker(token string) string {
	return "[[quack:advisor-thread:" + token + "]]"
}

// ParseAdvisorThread: extracts the LAST token from prompt text.
func ParseAdvisorThread(text string) (token string, ok bool) {
	ms := advisorMarkerRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return "", false
	}
	return ms[len(ms)-1][1], true
}

// AdvisorTask: seeds the mentor's first consult (task+rubric) + session coords.
type AdvisorTask struct {
	Task            string
	Rubric          string
	NodeID          string // for cancel/steer controls
	WorkspaceNodeID string // fs/git tool scope; shared across setup chain
	WorktreeParent  string // setup clone's scope for worktree nodes
	ReadOnly        bool   // this node's effective vetting.Config.ReadOnly (#754: sandbox grant, not just prompt)
	AppName         string
	UserID          string
	SessionID       string // ADK session id (sessions.Get lookups only - diverges from ChatID on a retry, see ChatID)
	ChatID          string // workspace/jail scope - the real chat id, stable across a retry's synthetic ADK session
	InvocationID    string
	MemSecret       string // unguessable per-node credential for ACP memory
	ACPSessionID    string // last round's ACP protocol session id, for cross-round resume (judge -> revise -> revise)
	// Round/TurnID/HeadSHA: the gate's own per-round coordinates, refreshed at
	// the start of every round (SetAdvisorThreadRound) so a tool-initiated write (write_finding et al, via the MemSession's AdvisorToken) stamps
	// real lineage instead of Round:0/TurnID:""/HeadSHA:"" - BuildReviewPreload drops any finding with an empty HeadSHA (#1091 adversarial review finding #4).
	Round   int
	TurnID  string
	HeadSHA string
	// TriggerAnnotation: the prior round's judge_round id, refreshed alongside
	// Round/TurnID/HeadSHA (SetAdvisorThreadRound) so a tool-initiated write
	// carries the same trigger_annotation chain as gate-written artifacts (design V4 §7 case 3, #1092).
	TriggerAnnotation string
}

// MemSession: ACP memory MCP resolution for one node.
type MemSession struct {
	Memory     *memory.Store
	Scope      memory.Scope
	Staged     *MemStage    // stage_memory buffer
	Recalled   *RecallStage // recall_memory hits (#1255 P2)
	Review     *ReviewStage // non-nil for review-delivery nodes
	PRStage    *PRStage     // non-nil for implement-delivery nodes
	ExistingPR bool         // PRStage != nil and the run pushes onto an already-open PR - offer stage_push, not stage_pr
	// Artifacts/AppName/UserID/ChatID: read_artifact scope. nil Artifacts
	// disables the tool; AppName/UserID/ChatID are never client-supplied,
	// so a node can only ever read its own chat's artifacts.
	Artifacts artifact.Service
	AppName   string
	UserID    string
	ChatID    string
	// Ledger: same fail-closed WAL path as recordClient's cfg.Ledger, so a
	// tool-initiated write records parent_revision like a gate write (#1153).
	Ledger ledger.LedgerStore
	// NodeID stamps Lineage.NodeID on writes made through list_artifacts/
	// edit_artifact/write_artifact/write_<kind> - provenance only, never
	// part of an artifact's id (#1090 §4.1).
	NodeID string
	// AdvisorToken looks up this node's AdvisorTask for its current
	// Round/TurnID/HeadSHA (SetAdvisorThreadRound) - the MCP handlers stamp
	// tool-initiated writes with these instead of hardcoding zero values (#1091 adversarial review finding #4).
	AdvisorToken string
	// ToolWritten records every id written via any loopback MCP artifact-write
	// tool this round (write_<kind>, write_artifact, edit_artifact), so
	// saveCodeReviewRound's answer-tail fallback and saveTextRound's tool-wrote check can both tell a tool-written id apart from one only known from a tail parse (#1091 finding #1, #1095 review finding #1).
	ToolWritten *ToolWrittenStage
}

// ToolWrittenStage: per-node record of ids written via any artifact-write MCP
// tool this round - drained (opposite of ReviewStage's snapshot-not-drain
// shape) by resetToolWrittenIDs at the top of saveCodeReviewRound/saveTextRound.
type ToolWrittenStage struct {
	mu  sync.Mutex
	ids map[string]bool
}

// NewToolWrittenStage builds an empty stage for one node.
func NewToolWrittenStage() *ToolWrittenStage { return &ToolWrittenStage{ids: map[string]bool{}} }

// Add records id as written via a tool call this round.
func (s *ToolWrittenStage) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	s.ids[id] = true
}

// Snapshot returns a copy of every id recorded so far.
func (s *ToolWrittenStage) Snapshot() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.ids))
	for id := range s.ids {
		out[id] = true
	}
	return out
}

// Reset returns every id recorded so far and clears the stage - the
// snapshot-then-drain scoping saveCodeReviewRound needs so an id written in
// round N doesn't wrongly suppress round N+1's write for the same id (#1108 finding 2: the stage previously had no reset and accumulated for the whole node run).
func (s *ToolWrittenStage) Reset() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.ids))
	for id := range s.ids {
		out[id] = true
	}
	s.ids = map[string]bool{}
	return out
}

// MemStage: per-node staging buffer for stage_memory.
type MemStage struct {
	mu    sync.Mutex
	items []memory.Candidate
}

// StagedReviewComment: one staged inline finding, id = "<path>:<line>#<n>".
type StagedReviewComment struct {
	ID string
	ReviewComment
}

// ReviewStage: per-node staging for review MCP surface. Snapshot-read, not drained.
type ReviewStage struct {
	mu       sync.Mutex
	event    string
	takeaway string
	verified []string
	notes    []string
	set      bool
	comments []StagedReviewComment
	seq      map[string]int // "path:line" → highest #n; stale ids error rather than resolving to wrong comment
	// fanout: non-nil only for a reviewer node in a multi-reviewer plan
	// (#867) - staging seam defense-in-depth, mirrors cfg.ReviewFanout.
	fanout *ReviewFanout
}

// NewReviewStage builds a review stage for one node. fanout is nil for
// single-reviewer plans (no early-approve guard needed).
func NewReviewStage(fanout *ReviewFanout) *ReviewStage {
	return &ReviewStage{fanout: fanout}
}

// IsNonDeliveringSlice reports whether this node feeds a downstream
// synthesizer (#1148) - mirrors node.go's isNonDeliveringSlice(cfg), the
// only other place this same fact is derived. Callers use it to withhold the verdict tools (stage_review/write_code_review) instead of registering them and refusing the call.
func (s *ReviewStage) IsNonDeliveringSlice() bool {
	return s.fanout != nil && s.fanout.SynthExpected()
}

// AddComment stages a finding, or - when the same path/line/body is already
// staged (dup=true) - returns the existing id instead of a second copy. A
// different body at the same line is a distinct finding, not a duplicate.
func (s *ReviewStage) AddComment(path string, line int, body string) (id string, dup bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == nil {
		s.seq = make(map[string]int)
	}
	key := fmt.Sprintf("%s:%d", path, line)
	if s.seq[key] > 0 {
		for _, c := range s.comments {
			if c.Path == path && c.Line == line && c.Body == body {
				return c.ID, true
			}
		}
	}
	s.seq[key]++
	id = fmt.Sprintf("%s#%d", key, s.seq[key])
	s.comments = append(s.comments, StagedReviewComment{ID: id, ReviewComment: ReviewComment{Path: path, Line: line, Body: body}})
	return id, false
}

func (s *ReviewStage) ListComments() []StagedReviewComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StagedReviewComment(nil), s.comments...)
}

func (s *ReviewStage) RemoveComment(id string) (ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.comments[:0]
	for _, c := range s.comments {
		if c.ID == id {
			ok = true
			continue
		}
		out = append(out, c)
	}
	s.comments = out
	return ok
}

// SetVerdict stages the overall event+takeaway/verified/notes. Refuses to stage "approve" while sibling reviewer nodes are still running (#867
// defense-in-depth) - a request_changes may still stage early, since it can only ever tighten the run's worst-of verdict. The refusal text never says
// "wait": a model reading it as an instruction is exactly how #1148's sleep-poll loop started. Caps on takeaway/verified/notes are enforced at the tool boundary (reviewmcp.go's stage_review), not here - this is a plain store.
func (s *ReviewStage) SetVerdict(event, takeaway string, verified, notes []string) error {
	if event == "approve" && s.fanout != nil && s.fanout.SiblingsPending() {
		return fmt.Errorf("approve cannot be staged while sibling reviewer nodes are running; " +
			"this node's verdict is not delivered - finish your reply without one")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event, s.takeaway, s.verified, s.notes, s.set = event, takeaway, verified, notes, true
	return nil
}

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
	comments := make([]ReviewComment, len(s.comments))
	for i, c := range s.comments {
		comments[i] = c.ReviewComment
	}
	// Rendered fallback body: used verbatim only when no code_review
	// artifact backs this delivery (artifactRenderedDelivery falls back to
	// this staged text) - no scope/since-last-review info available here, so those sections are simply omitted (renderReviewOverview).
	body := renderReviewOverview(reviewOverviewInput{Verdict: event, Takeaway: s.takeaway, Verified: s.verified, Notes: s.notes, Comments: comments})
	return StagedDelivery{
		Kind:     "review",
		Event:    event,
		Body:     body,
		Comments: comments,
		Takeaway: s.takeaway,
		Verified: s.verified,
		Notes:    s.notes,
	}, true
}

// PRStage stages the pull-request delivery item, from either stage_pr (both
// fields required - opens a new PR) or stage_push (both optional - pushes
// onto one already open). Only one of the two is ever registered for a given node (internal/acp/acp.go's mcpToolNames), so Set/SetPush are never both called in the same run.
type PRStage struct {
	mu           sync.Mutex
	title        string
	body         string
	titleOmitted bool
	bodyOmitted  bool
	set          bool
}

// Set stages a full PR title+body (stage_pr - the caller already validated both non-empty).
func (s *PRStage) Set(title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title, s.body, s.set = title, body, true
	s.titleOmitted, s.bodyOmitted = false, false
}

// SetPush stages a push against an existing PR (stage_push); hasTitle/hasBody
// false means the agent omitted that field - it stays untouched at delivery.
func (s *PRStage) SetPush(title string, hasTitle bool, body string, hasBody bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title, s.titleOmitted = title, !hasTitle
	s.body, s.bodyOmitted = body, !hasBody
	s.set = true
}

func (s *PRStage) Snapshot() (StagedDelivery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return StagedDelivery{}, false
	}
	return StagedDelivery{
		Kind: "pull_request", Title: s.title, Body: s.body,
		TitleOmitted: s.titleOmitted, BodyOmitted: s.bodyOmitted,
	}, true
}

func (s *MemStage) Add(c memory.Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, c)
}

func (s *MemStage) Drain() []memory.Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.items
	s.items = nil
	return out
}

// RecallStage: per-node collector for the ACP loopback MCP's recall_memory
// calls (epic #1255 P2). Unlike MemStage, it's Snapshot-read (not Drain'd):
// an ACP worker's tool calls are otherwise invisible to this session, so the round loop needs to see hits so far EVERY round, not just once at the end.
type RecallStage struct {
	mu   sync.Mutex
	hits []memory.Delivered
}

func (s *RecallStage) Add(hits ...memory.Delivered) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, hits...)
}

// Snapshot returns every hit collected so far, without clearing it.
func (s *RecallStage) Snapshot() []memory.Delivered {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memory.Delivered, len(s.hits))
	copy(out, s.hits)
	return out
}

// NewMemSecret: fresh unguessable per-node credential (256 bits, hex).
func NewMemSecret() (string, error) {
	var b [32]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

var memSessions sync.Map // secret → MemSession

func RegisterMemSession(secret string, s MemSession) {
	if secret == "" {
		return
	}
	memSessions.Store(secret, s)
}

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

var memSessionsConnected sync.Map // secrets that had a loopback MCP request

func MarkMemSessionConnected(secret string) {
	if secret == "" {
		return
	}
	memSessionsConnected.Store(secret, struct{}{})
}

func UnregisterMemSession(secret string) {
	if secret == "" {
		return
	}
	if _, existed := memSessions.LoadAndDelete(secret); !existed {
		return
	}
	if _, connected := memSessionsConnected.LoadAndDelete(secret); !connected {
		slog.Warn("acp: loopback MCP session torn down having never connected - tools were offered but unreachable", "component", "vetting")
	}
}

var advisorThreads sync.Map // token → AdvisorTask

func RegisterAdvisorThread(token string, t AdvisorTask) {
	advisorThreads.Store(token, t)
}

func LookupAdvisorThread(token string) (AdvisorTask, bool) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return AdvisorTask{}, false
	}
	t, ok := v.(AdvisorTask)
	return t, ok
}

func UnregisterAdvisorThread(token string) {
	advisorThreads.Delete(token)
	if NodeSessionClosed != nil {
		NodeSessionClosed(token)
	}
}

// NodeSessionClosed, when set, runs at the end of UnregisterAdvisorThread -
// lets acp release a pinned process without vetting importing acp (acp
// already imports vetting the other way).
var NodeSessionClosed func(token string)

// SetAdvisorThreadSessionID records the ACP session id a round established,
// so the next round for this same node (judge -> revise -> revise) can
// resume it instead of starting a cold session (#1006 tool-call amnesia).
func SetAdvisorThreadSessionID(token, sessionID string) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return
	}
	t := v.(AdvisorTask)
	t.ACPSessionID = sessionID
	advisorThreads.Store(token, t)
}

// SetAdvisorThreadRound records the gate's current round/turn/head-sha coordinates on token's AdvisorTask - called at the start of every judge
// round (and once for the draft) so a tool-initiated write made during that
// round (write_finding et al, looked up via the MemSession's AdvisorToken) stamps real lineage instead of zero values (#1091 adversarial review finding #4).
func SetAdvisorThreadRound(token string, round int, turnID, headSHA, triggerAnnotation string) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return
	}
	t := v.(AdvisorTask)
	t.Round, t.TurnID, t.HeadSHA, t.TriggerAnnotation = round, turnID, headSHA, triggerAnnotation
	advisorThreads.Store(token, t)
}
