package vetting

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/memory"
)

// advisorMarkerRe extracts the token from the marker line.
var advisorMarkerRe = regexp.MustCompile(`\[\[quack:advisor-thread:([^\]]+)\]\]`)

// AdvisorThreadToken: stable per-node token.
func AdvisorThreadToken(planID, nodeID string) string {
	return planID + "/" + nodeID
}

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
}

// MemSession: ACP memory MCP resolution for one node.
type MemSession struct {
	Memory     *memory.Store
	Scope      memory.Scope
	Staged     *MemStage    // stage_memory buffer
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
	body     string
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

func (s *ReviewStage) AddComment(path string, line int, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == nil {
		s.seq = make(map[string]int)
	}
	key := fmt.Sprintf("%s:%d", path, line)
	s.seq[key]++
	id := fmt.Sprintf("%s#%d", key, s.seq[key])
	s.comments = append(s.comments, StagedReviewComment{ID: id, ReviewComment: ReviewComment{Path: path, Line: line, Body: body}})
	return id
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

// SetVerdict stages the overall event+body. Refuses to stage "approve"
// while sibling reviewer nodes are still running (#867 defense-in-depth) -
// a request_changes may still stage early, since it can only ever tighten
// the run's worst-of verdict.
func (s *ReviewStage) SetVerdict(event, body string) error {
	if event == "approve" && s.fanout != nil && s.fanout.SiblingsPending() {
		return fmt.Errorf("a sibling reviewer node is still running - only request_changes may stage early; " +
			"approve waits until every reviewer node in this run has finished")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event, s.body, s.set = event, body, true
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
	return StagedDelivery{
		Kind:     "review",
		Event:    event,
		Body:     s.body,
		Comments: comments,
	}, true
}

// PRStage stages the pull-request delivery item, from either stage_pr (both
// fields required - opens a new PR) or stage_push (both optional - pushes
// onto one that's already open). Only one of the two is ever registered for
// a given node (internal/acp/acp.go's mcpToolNames), so Set/SetPush are never
// both called in the same run.
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
}

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
