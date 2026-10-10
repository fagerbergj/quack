package vetting

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

func AdvisorThreadToken(planID, nodeID string) string {
	return planID + "/" + nodeID
}

// AdvisorTask: per-node identity/session coords, keyed by thread token; a node's loopback MCP
// tools (memory, artifacts, ToolWritten) resolve their scope from it.
type AdvisorTask struct {
	Task            string
	Rubric          string
	NodeID          string // for cancel/steer controls
	WorkspaceNodeID string // fs/git tool scope; shared across setup chain
	WorktreeParent  string // setup clone's scope for worktree nodes
	ReadOnly        bool   // this node's effective vetting.Config.ReadOnly (a sandbox grant, not just prompt)
	AppName         string
	UserID          string
	SessionID       string // ADK session id for sessions.Get; diverges from ChatID on a retry
	ChatID          string // workspace/jail scope - the real chat id, stable across a retry's synthetic ADK session
	InvocationID    string
	MemSecret       string // unguessable per-node credential for ACP memory
	ACPSessionID    string // last round's ACP protocol session id, for cross-round resume (judge -> revise -> revise)
	// Round/TurnID/HeadSHA: the gate's per-round coords, refreshed every round so tool-initiated writes
	// stamp real lineage; BuildReviewPreload drops any finding with an empty HeadSHA.
	Round   int
	TurnID  string
	HeadSHA string
	// TriggerAnnotation: the prior round's judge_round id, so a tool-initiated write carries the same
	// trigger_annotation chain as gate-written artifacts.
	TriggerAnnotation string

	// AllowedDeliveryKinds mirrors vetting.Config's: nil = unrestricted, non-nil empty = deny-all.
	AllowedDeliveryKinds []string
	PlanOnly             bool // the run's deliverable is a plan (quack:plan): nothing may be written or posted
}

// MemSession: ACP memory MCP resolution for one node.
type MemSession struct {
	Memory     *memory.Store
	Scope      memory.Scope
	Staged     *MemStage    // stage_memory buffer
	Recalled   *RecallStage // recall_memory hits
	Review     *ReviewStage // non-nil for review-delivery nodes
	PRStage    *PRStage     // non-nil for implement-delivery nodes
	ExistingPR bool         // run pushes onto an already-open PR: offer stage_push, not stage_pr
	// Artifacts/AppName/UserID/ChatID: read_artifact scope; nil Artifacts disables it. Never
	// client-supplied, so a node can only read its own chat's artifacts.
	Artifacts artifact.Service
	AppName   string
	UserID    string
	ChatID    string
	// Ledger: same fail-closed WAL path as recordClient's cfg.Ledger, so a
	// tool-initiated write records parent_revision like a gate write.
	Ledger ledger.LedgerStore
	// Schemas: same registered-schema enforcement as recordClient's cfg.Schemas.
	Schemas *artifactschema.Registry
	// NodeID stamps Lineage.NodeID on artifact-tool writes: provenance only, never part of an id.
	NodeID string
	// AdvisorToken looks up this node's AdvisorTask so MCP handlers stamp tool-initiated writes
	// with the current Round/TurnID/HeadSHA, not zero values.
	AdvisorToken string
	// ToolWritten records every id written via a loopback MCP artifact-write tool this round, so
	// the round savers can tell a tool-written id from one only known from a tail parse.
	ToolWritten *ToolWrittenStage
}

// ToolWrittenStage: per-node ids written via an artifact-write MCP tool this round; drained
// by resetToolWrittenIDs each round, unlike ReviewStage.
type ToolWrittenStage struct {
	mu  sync.Mutex
	ids map[string]bool
}

func NewToolWrittenStage() *ToolWrittenStage { return &ToolWrittenStage{ids: map[string]bool{}} }

func (s *ToolWrittenStage) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	s.ids[id] = true
}

func (s *ToolWrittenStage) Snapshot() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.ids))
	for id := range s.ids {
		out[id] = true
	}
	return out
}

// Reset returns and clears every id recorded, so an id written in round N doesn't suppress
// round N+1's write for the same id.
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
	// fanout: non-nil only in a multi-reviewer plan; mirrors cfg.ReviewFanout as defense-in-depth.
	fanout *ReviewFanout
}

// NewReviewStage: fanout is nil for single-reviewer plans (no early-approve guard needed).
func NewReviewStage(fanout *ReviewFanout) *ReviewStage {
	return &ReviewStage{fanout: fanout}
}

// IsNonDeliveringSlice: this node feeds a downstream synthesizer (mirrors isNonDeliveringSlice(cfg)).
// Callers withhold the verdict tools rather than register them and refuse the call.
func (s *ReviewStage) IsNonDeliveringSlice() bool {
	return s.fanout != nil && s.fanout.SynthExpected()
}

// AddComment stages a finding, or returns the existing id (dup=true) when the same
// path/line/body is already staged; a different body at the same line is distinct.
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

// SetVerdict refuses "approve" while sibling reviewers still run (request_changes only tightens).
// The refusal never says "wait": models read that as an instruction and sleep-poll.
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
	// Fallback body, used only when no code_review artifact backs this delivery; scope and
	// since-last-review sections are omitted since that info isn't available here.
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

// PRStage stages the PR delivery item from stage_pr (new PR) or stage_push (open PR). Only one
// of the two is registered per node, so Set and SetPush never both run.
type PRStage struct {
	mu           sync.Mutex
	title        string
	body         string
	titleOmitted bool
	bodyOmitted  bool
	set          bool
}

// Set: stage_pr; the caller already validated both non-empty.
func (s *PRStage) Set(title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title, s.body, s.set = title, body, true
	s.titleOmitted, s.bodyOmitted = false, false
}

// SetPush: stage_push; hasTitle/hasBody false leaves that field untouched at delivery.
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

// RecallStage collects the ACP loopback MCP's recall_memory hits. Snapshot-read, not drained:
// ACP tool calls are invisible to this session, so every round needs the hits so far.
type RecallStage struct {
	mu   sync.Mutex
	hits []memory.Delivered
}

func (s *RecallStage) Add(hits ...memory.Delivered) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, hits...)
}

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

type advisorTokenKey struct{}

// WithAdvisorToken marks ctx as acting for node token: dag stamps worker rounds, the gate judge rounds.
func WithAdvisorToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, advisorTokenKey{}, token)
}

// AdvisorTokenFromContext is the token WithAdvisorToken set on ctx, "" if none.
func AdvisorTokenFromContext(ctx context.Context) string {
	s, _ := ctx.Value(advisorTokenKey{}).(string)
	return s
}

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

// NodeSessionClosed runs at the end of UnregisterAdvisorThread, letting acp release a pinned
// process without an import cycle.
var NodeSessionClosed func(token string)

// SetAdvisorThreadSessionID records the round's ACP session id so the node's next round resumes
// it instead of starting cold (which loses tool-call memory).
func SetAdvisorThreadSessionID(token, sessionID string) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return
	}
	t := v.(AdvisorTask)
	t.ACPSessionID = sessionID
	advisorThreads.Store(token, t)
}

// SetAdvisorThreadRound records the current round coords on token's AdvisorTask (draft and each
// judge round), so tool-initiated writes stamp real lineage instead of zero values.
func SetAdvisorThreadRound(token string, round int, turnID, headSHA, triggerAnnotation string) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return
	}
	t := v.(AdvisorTask)
	t.Round, t.TurnID, t.HeadSHA, t.TriggerAnnotation = round, turnID, headSHA, triggerAnnotation
	advisorThreads.Store(token, t)
}
