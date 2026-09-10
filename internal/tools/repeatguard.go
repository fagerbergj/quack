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
// threshold, then force-ends the node's turn once the model has ignored the
// refusal hardStopAfter times.
type repeatGuard struct {
	inner         runnableTool
	states        *repeatStates
	threshold     int
	hardStopAfter int
	// endTurn aborts the round via dag.Executor.NoteToolLoopFailure - a
	// returned tool error alone can't: ADK folds it into a function response
	// and keeps the model's turn going.
	endTurn func(chatID, nodeID, msg string) bool
}

// outcomeDeclinedKey: a tool's result may set this (bool true) to signal a
// failure that isn't a Go error - e.g. ask_advisor's consult failing
// internally but still returning a graceful empty-advice result to the
// model. Read here, then stripped so the model never sees it.
const outcomeDeclinedKey = "quack_tool_declined"

// defaultRepeatThreshold: consecutive identical calls before refusal.
// defaultHardStopAfterRefusals: further refusals (beyond the first) the
// model can ignore before the node's turn is force-ended.
const (
	defaultRepeatThreshold       = 3
	defaultHardStopAfterRefusals = 2
)

// repeatStates: tracks the last call fingerprint per session.
// ponytail: entries never pruned - add if sessions number in the millions.
type repeatStates struct {
	mu    sync.Mutex
	last  map[string]*repeatState
	fails map[string]int
}

type repeatState struct {
	fingerprint string
	count       int
}

func newRepeatStates() *repeatStates {
	return &repeatStates{last: map[string]*repeatState{}, fails: map[string]int{}}
}

// observe records a call, returns the consecutive count for this fingerprint.
func (s *repeatStates) observe(sessionID, fingerprint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.last[sessionID]
	if st == nil || st.fingerprint != fingerprint {
		st = &repeatState{fingerprint: fingerprint}
		s.last[sessionID] = st
	}
	st.count++
	return st.count
}

// resetSession clears a session's streak counter after a hard stop, so a
// retry (e.g. a revise round) starts with a fresh budget instead of
// immediately hard-stopping again on its very first call.
func (s *repeatStates) resetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.last, sessionID)
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

func newRepeatGuard(inner tool.Tool, states *repeatStates, threshold, hardStopAfter int, endTurn func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support repeat guarding (not a runnable function tool)", inner.Name())
	}
	if threshold <= 0 {
		threshold = defaultRepeatThreshold
	}
	if hardStopAfter <= 0 {
		hardStopAfter = defaultHardStopAfterRefusals
	}
	return &repeatGuard{inner: rt, states: states, threshold: threshold, hardStopAfter: hardStopAfter, endTurn: endTurn}, nil
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

// Run: refuses the threshold'th+ consecutive identical call, and force-ends
// the node's turn once the model has ignored that refusal hardStopAfter times.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.inner.Run(ctx, args) // unfingerprintable args: never block
	}
	sessionID := ctx.SessionID()
	fingerprint := g.Name() + ":" + string(argsJSON)

	n := g.states.observe(sessionID, fingerprint)
	if n > g.threshold+g.hardStopAfter {
		msg := fmt.Sprintf("tool-call loop: %s called with identical arguments %d consecutive times despite being refused; node terminated", g.Name(), n)
		slog.Warn("tool call loop: ending node turn", "component", "tools",
			"tool", g.Name(), "consecutive", n, "session", sessionID)
		g.endNodeTurn(ctx, msg)
		g.states.resetSession(sessionID) // a retry (e.g. revise) starts with a fresh budget, not an already-blown one
		return nil, errors.New(msg)
	}
	if n >= g.threshold {
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
	failed := runErr != nil
	if v, ok := result[outcomeDeclinedKey]; ok {
		delete(result, outcomeDeclinedKey) // internal signal (e.g. ask_advisor's declined consult) - never model-facing
		if declined, ok := v.(bool); ok && declined {
			failed = true
		}
	}
	if hasResource {
		g.states.observeResourceFail(sessionID, resourceKey, failed)
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
func repeatWrap(t tool.Tool, states *repeatStates, threshold, hardStopAfter int, endTurn func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	return newRepeatGuard(t, states, threshold, hardStopAfter, endTurn)
}
