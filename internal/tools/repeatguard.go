package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// repeatGuard breaks identical-call loops: a model that re-issues the exact
// same tool call (name + args) consecutively is not gathering new
// information - it has wedged on a repetition attractor (a live case wedged
// nodes for hours: 611 identical `git log` calls in a row). Sampling
// penalties can't reliably reach across large tool results, so from the
// third consecutive identical call, the call is REFUSED with a steering
// error (refusal text varies, so refusals themselves never loop). The
// gate's node-level caps remain the hard stop; this steers the model out
// while its context is still small.
type repeatGuard struct {
	inner  runnableTool
	states *repeatStates
}

// repeatThreshold is the consecutive-identical-call count at which refusal
// starts: the 1st call runs, an immediate identical retry (2nd) is allowed -
// legitimate after a transient error - and the 3rd is refused.
const repeatThreshold = 3

// repeatStates tracks the last call fingerprint per session, shared by every
// tool of one registry build so consecutiveness is judged across tools (call A,
// call B, call A is NOT a repeat). Keyed by ctx.SessionID() - per-run A2A
// sessions, so state resets naturally with each run.
// ponytail: entries are never pruned - a few dozen bytes per session on a
// single-tenant bot; add pruning if sessions ever number in the millions.
type repeatStates struct {
	mu   sync.Mutex
	last map[string]*repeatState
	// fails counts consecutive FAILED calls per session+(tool, resource) - see
	// pathFailThreshold. Keyed sessionID+"|"+tool+":"+resource.
	fails map[string]int
}

type repeatState struct {
	fingerprint string
	count       int
}

func newRepeatStates() *repeatStates {
	return &repeatStates{last: map[string]*repeatState{}, fails: map[string]int{}}
}

// observe records a call and returns how many times this exact fingerprint has
// now been issued consecutively in the session.
func (s *repeatStates) observe(sessionID, fingerprint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.last[sessionID]
	if st == nil || st.fingerprint != fingerprint {
		s.last[sessionID] = &repeatState{fingerprint: fingerprint, count: 1}
		return 1
	}
	st.count++
	return st.count
}

// resourceFailCount returns the current consecutive-failure count for
// sessionID+resourceKey, without mutating it.
func (s *repeatStates) resourceFailCount(sessionID, resourceKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails[sessionID+"|"+resourceKey]
}

// observeResourceFail records the outcome of a call against resourceKey: a
// success (failed=false) resets the streak to 0; a failure increments it and
// returns the new count.
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

// newRepeatGuard wraps inner. Same runnableTool requirement (and reason) as
// newCancelGuard: fail loudly at build time rather than ship an unguardable tool.
func newRepeatGuard(inner tool.Tool, states *repeatStates) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support repeat guarding (not a runnable function tool)", inner.Name())
	}
	return &repeatGuard{inner: rt, states: states}, nil
}

func (g *repeatGuard) Name() string        { return g.inner.Name() }
func (g *repeatGuard) Description() string { return g.inner.Description() }
func (g *repeatGuard) IsLongRunning() bool { return g.inner.IsLongRunning() }

func (g *repeatGuard) Declaration() *genai.FunctionDeclaration { return g.inner.Declaration() }

// ProcessRequest re-points the dispatch entry at the wrapper - same shape and
// reason as cancelGuard.ProcessRequest.
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

// pathFailThreshold is the consecutive-FAILED-call count against the same
// (tool, resource) - regardless of other args - before the NEXT call
// against it is refused outright. Catches the semantic-churn case the
// byte-identical guard misses: varying args each try, never repeating
// exactly, but never learning the resource itself is the problem. A failed
// call always runs (its error is informative); only once pathFailThreshold
// have run consecutively does the guard refuse the next one unexecuted.
// Deliberately narrower than fuzzy call-similarity (rejected: false-positive
// risk on legitimate paging/iteration); only covers tools with a `path` or
// `url` arg. Upgrade path on a false positive: also require the failure's
// error text to repeat, not just the (tool, resource) pair.
const pathFailThreshold = 3

// Run executes the call unless it trips one of two loop breakers:
//  1. repeatThreshold-th (or later) consecutive byte-identical call.
//  2. the (pathFailThreshold+1)-th consecutive call against a (tool, resource)
//     that has already failed pathFailThreshold times in a row - semantic churn.
//
// Either returns a steering error the model can act on instead of the result.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.inner.Run(ctx, args) // unfingerprintable args: never block
	}
	sessionID := ctx.SessionID()

	n := g.states.observe(sessionID, g.Name()+":"+string(argsJSON))
	if n >= repeatThreshold {
		slog.Warn("tool call refused: identical call repeated", "component", "tools",
			"tool", g.Name(), "consecutive", n, "session", sessionID)
		return nil, fmt.Errorf(
			"REFUSED (attempt %d): this is the %dth consecutive time you issued this exact %s call with these exact arguments. "+
				"Its result has not changed - it is already in the conversation above. Re-issuing it again will be refused again. "+
				"Take a DIFFERENT action: use the result you already have, try a different tool or different arguments, or if you "+
				"are finished, stop calling tools and write your final answer now.",
			n-repeatThreshold+1, n, g.Name())
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
	if hasResource {
		g.states.observeResourceFail(sessionID, resourceKey, runErr != nil)
	}
	return result, runErr
}

// resourceFingerprint extracts a stable per-call resource identity from a
// tool call's marshaled args - the `path` or `url` field, if either is a
// non-empty string. Reports false when neither is present (unmarshalable args,
// or a tool whose calls aren't resource-scoped), so callers can skip the
// failure-streak check rather than fingerprint on the whole args blob (which
// would just reduce to the byte-identical guard above).
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

// repeatWrap applies the identical-call breaker. Sits just inside the cancel
// guard (a cancelled node's refusal wins) and outside the guard ladder (a
// refused repeat must not consume a judge/confirm round).
func repeatWrap(t tool.Tool, states *repeatStates) (tool.Tool, error) {
	return newRepeatGuard(t, states)
}
