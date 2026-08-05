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

// repeatGuard: breaks identical-call loops - refuses the 3rd+ consecutive identical call.
type repeatGuard struct {
	inner  runnableTool
	states *repeatStates
}

// repeatThreshold: consecutive identical calls before refusal (1st=run, 2nd=retry, 3rd=refused).
const repeatThreshold = 3

// repeatStates: tracks last call fingerprint per session.
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

// observe records a call, returns consecutive count for this fingerprint.
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

// newRepeatGuard wraps inner, fails loudly if not runnable.
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

// Run: refuses if byte-identical triple or resource-failure churn.
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
func repeatWrap(t tool.Tool, states *repeatStates) (tool.Tool, error) {
	return newRepeatGuard(t, states)
}
