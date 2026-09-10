package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// repeatGuard: breaks identical-call loops - refuses a call past its
// threshold, then force-ends the node's turn if the model repeats it anyway.
type repeatGuard struct {
	inner         runnableTool
	states        *repeatStates
	failThresh    int
	successThresh int
	maxCalls      int
	// endTurn aborts the round via dag.Executor.NoteToolLoopFailure - a
	// returned tool error alone can't: ADK folds it into a function response
	// and keeps the model's turn going.
	endTurn func(chatID, nodeID, msg string) bool
}

// failThreshold/successThreshold: consecutive identical calls before the
// node's turn is force-ended (1..threshold-1 run, threshold is refused,
// threshold+1 force-ends it). A streak whose last outcome errored is cut off
// sooner than one that kept succeeding (e.g. polling, or a harmless re-read).
const (
	defaultFailThreshold    = 3
	defaultSuccessThreshold = 8
	// defaultMaxToolCalls: per-node total tool-call ceiling backstop (any tool, any args).
	defaultMaxToolCalls = 200
)

// repeatStates: tracks the last call fingerprint and total call count per session.
// ponytail: entries never pruned - add if sessions number in the millions.
type repeatStates struct {
	mu     sync.Mutex
	last   map[string]*repeatState
	fails  map[string]int
	totals map[string]int
}

type repeatState struct {
	fingerprint string
	count       int
	failed      bool // outcome of the last actually-executed call with this fingerprint
}

func newRepeatStates() *repeatStates {
	return &repeatStates{last: map[string]*repeatState{}, fails: map[string]int{}, totals: map[string]int{}}
}

// observe records a call, returns the consecutive count for this fingerprint
// and the threshold that applies to it (picked from the fingerprint's last
// executed outcome, not this call - a call this refuses is never executed).
func (s *repeatStates) observe(sessionID, fingerprint string, failThresh, successThresh int) (n, threshold int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.last[sessionID]
	if st == nil || st.fingerprint != fingerprint {
		st = &repeatState{fingerprint: fingerprint}
		s.last[sessionID] = st
	}
	st.count++
	threshold = successThresh
	if st.failed {
		threshold = failThresh
	}
	return st.count, threshold
}

// recordOutcome updates the fingerprint's last-executed outcome, used to pick
// the threshold on the NEXT identical call. No-op if a different call landed
// in between (the streak already broke).
func (s *repeatStates) recordOutcome(sessionID, fingerprint string, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.last[sessionID]; st != nil && st.fingerprint == fingerprint {
		st.failed = failed
	}
}

// observeTotal increments and returns the session's total tool-call count
// across every tool and fingerprint - the generous backstop for a loop the
// identical-args check doesn't catch.
func (s *repeatStates) observeTotal(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals[sessionID]++
	return s.totals[sessionID]
}

// resourceFailCount returns consecutive-failure count without mutating.
func (s *repeatStates) resourceFailCount(sessionID, resourceKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails[sessionID+"|"+resourceKey]
}

// observeResourceFail records call outcome: success resets, failure increments.
func (s *repeatStates) observeResourceFail(sessionID, resourceKey string, failed bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := sessionID + "|" + resourceKey
	if !failed {
		delete(s.fails, k)
		return 0
	}
	s.fails[k]++
	return s.fails[k]
}

func newRepeatGuard(inner tool.Tool, states *repeatStates, failThresh, successThresh, maxCalls int, endTurn func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support repeat guarding (not a runnable function tool)", inner.Name())
	}
	if failThresh <= 0 {
		failThresh = defaultFailThreshold
	}
	if successThresh <= 0 {
		successThresh = defaultSuccessThreshold
	}
	if maxCalls <= 0 {
		maxCalls = defaultMaxToolCalls
	}
	return &repeatGuard{inner: rt, states: states, failThresh: failThresh, successThresh: successThresh, maxCalls: maxCalls, endTurn: endTurn}, nil
}

func (g *repeatGuard) Name() string        { return g.inner.Name() }
func (g *repeatGuard) Description() string { return g.inner.Description() }
func (g *repeatGuard) IsLongRunning() bool { return g.inner.IsLongRunning() }

// SetLedgerCoords: pass-through wrapper, forward to inner (#1052).
func (g *repeatGuard) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := g.inner.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(c)
	}
}

func (g *repeatGuard) Declaration() *genai.FunctionDeclaration { return g.inner.Declaration() }

// ProcessRequest re-points dispatch at the wrapper.
func (g *repeatGuard) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := g.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[g.Name()]; ok {
			req.Tools[g.Name()] = g
		}
	}
	return nil
}

// pathFailThreshold: consecutive failures against a (tool, resource) before refusing the next call.
const pathFailThreshold = 3

// Run: refuses an identical call at its threshold, force-ends the node's
// turn if the model repeats it again, and separately force-ends the turn if
// the node's total tool-call count (any tool, any args) blows past its
// generous backstop ceiling.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.inner.Run(ctx, args) // unfingerprintable args: never block
	}
	sessionID := ctx.SessionID()
	fingerprint := g.Name() + ":" + string(argsJSON)

	if total := g.states.observeTotal(sessionID); total > g.maxCalls {
		msg := fmt.Sprintf("tool-call ceiling: this node has made %d tool calls this run (limit %d); node terminated", total, g.maxCalls)
		slog.Warn("tool call ceiling exceeded; ending node turn", "component", "tools", "session", sessionID, "total", total, "limit", g.maxCalls)
		g.endNodeTurn(ctx, msg)
		return nil, errors.New(msg)
	}

	n, threshold := g.states.observe(sessionID, fingerprint, g.failThresh, g.successThresh)
	switch {
	case n > threshold:
		msg := fmt.Sprintf("tool-call loop: %s called with identical arguments %d consecutive times despite being refused; node terminated", g.Name(), n)
		slog.Warn("tool call loop: ending node turn", "component", "tools",
			"tool", g.Name(), "consecutive", n, "session", sessionID)
		g.endNodeTurn(ctx, msg)
		return nil, errors.New(msg)
	case n == threshold:
		slog.Warn("tool call refused: identical call repeated", "component", "tools",
			"tool", g.Name(), "consecutive", n, "session", sessionID)
		return nil, fmt.Errorf(
			"REFUSED: this is the %dth consecutive time you issued this exact %s call with these exact arguments. "+
				"Its result has not changed - it is already in the conversation above. Re-issuing it again will END THIS NODE'S TURN "+
				"as a failure. Take a DIFFERENT action: use the result you already have, try a different tool or different arguments, "+
				"or if you are finished, stop calling tools and write your final answer now.",
			n, g.Name())
	}

	resource, hasResource := resourceFingerprint(argsJSON)
	resourceKey := g.Name() + ":" + resource
	if hasResource {
		if fails := g.states.resourceFailCount(sessionID, resourceKey); fails >= pathFailThreshold {
			slog.Warn("tool call refused: repeated failures against same resource", "component", "tools",
				"tool", g.Name(), "resource", resource, "consecutive_fails", fails, "session", sessionID)
			return nil, fmt.Errorf(
				"REFUSED: %s against %q has now failed %d times in a row with varying arguments. Varying the arguments "+
					"further is not working - the problem is with %q itself or your understanding of it, not the "+
					"specific call. Stop retrying this resource: inspect it a different way (e.g. list its "+
					"surroundings), pick a different resource, or report the failure in your final answer.",
				g.Name(), resource, fails, resource)
		}
	}

	result, runErr := g.inner.Run(ctx, args)
	g.states.recordOutcome(sessionID, fingerprint, runErr != nil)
	if hasResource {
		g.states.observeResourceFail(sessionID, resourceKey, runErr != nil)
	}
	return result, runErr
}

// endNodeTurn resolves the calling node from the advisor-thread marker
// (nodeScope, the same one cancelGuard uses) and aborts its in-flight round.
// No-op outside a gated node (nodeScope returns "") or without a wired
// executor (e.g. replay/test builds) - the returned tool error is the only
// signal in that case, same as before this guard existed.
func (g *repeatGuard) endNodeTurn(ctx agent.Context, msg string) {
	if g.endTurn == nil {
		return
	}
	if chatID, nodeID := nodeScope(ctx); nodeID != "" {
		g.endTurn(chatID, nodeID, msg)
	}
}

// resourceFingerprint extracts the `path` or `url` field from tool args for failure-streak tracking.
func resourceFingerprint(argsJSON []byte) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(argsJSON, &m); err != nil {
		return "", false
	}
	for _, key := range []string{"path", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// repeatWrap applies the identical-call breaker.
func repeatWrap(t tool.Tool, states *repeatStates, failThresh, successThresh, maxCalls int, endTurn func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	return newRepeatGuard(t, states, failThresh, successThresh, maxCalls, endTurn)
}
