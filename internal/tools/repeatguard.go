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
// same tool call (name + args) consecutively is not gathering new information —
// it has wedged on a repetition attractor. Measured in dogfooding with full
// request-tail instrumentation: the harness delivered every result and the
// model repeated anyway (611× `git log`, 26× the same read_file), wedging nodes
// for hours. Sampling penalties can't reliably reach across large tool results
// (one 20 KB read ≈ 5K tokens vs a finite penalty window), so the backstop
// lives here: from the third consecutive identical call, the call is REFUSED
// with a steering error instead of executed. The refusal text changes each time
// (attempt counter), so the refusals themselves never form an identical loop.
// The gate's node-level caps remain the hard stop; this guard exists to steer
// the model out of the loop while its context is still small.
type repeatGuard struct {
	inner  runnableTool
	states *repeatStates
}

// repeatThreshold is the consecutive-identical-call count at which refusal
// starts: the 1st call runs, an immediate identical retry (2nd) is allowed —
// legitimate after a transient error — and the 3rd is refused.
const repeatThreshold = 3

// repeatStates tracks the last call fingerprint per session, shared by every
// tool of one registry build so consecutiveness is judged across tools (call A,
// call B, call A is NOT a repeat). Keyed by ctx.SessionID() — per-run A2A
// sessions, so state resets naturally with each run.
// ponytail: entries are never pruned — a few dozen bytes per session on a
// single-tenant bot; add pruning if sessions ever number in the millions.
type repeatStates struct {
	mu   sync.Mutex
	last map[string]*repeatState

	// failedPaths tracks consecutive failed calls per (tool, path) combo.
	// Key is "tool:path" or just "tool" if no path is present. Value tracks
	// how many consecutive failures occurred for this combo. Reset on ANY success.
	failedPaths map[string]int
}

type repeatState struct {
	fingerprint string
	count       int
}

const repeatFailThresh = 3

func newRepeatStates() *repeatStates {
	return &repeatStates{
		last:        map[string]*repeatState{},
		failedPaths: map[string]int{},
	}
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

// resetFailedPaths clears all failure counters. Called on any tool success to
// break the chain of consecutive failures (one success resets all semantic churn).
func (s *repeatStates) resetFailedPaths() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.failedPaths {
		delete(s.failedPaths, k)
	}
}

// recordFailure records a failed call for the given key and returns the updated count.
func (s *repeatStates) recordFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedPaths[key]++
}

// getFailureCount returns the current failure count for the given key without
// incrementing it.
func (s *repeatStates) getFailureCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failedPaths[key]
}

// extractPath pulls the file path from tool args. Best-effort: tools like
// read_file, write_file, edit_file, delete_path, glob, grep, list_dir all use
// a "path" field; tools without one return empty (track by tool name only).
func extractPath(args any) string {
	if args == nil {
		return ""
	}
	m, ok := args.(map[string]any)
	if !ok {
		return ""
	}
	if p, ok := m["path"]; ok {
		if s, ok := p.(string); ok {
			return s
		}
	}
	return ""
}

// getFailureKey computes the key for tracking failures. Returns "tool:path" or
// just "tool" if no path is present.
func getFailureKey(toolName string, args any) string {
	key := toolName
	if p := extractPath(args); p != "" {
		key = toolName + ":" + p
	}
	return key
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

// ProcessRequest re-points the dispatch entry at the wrapper — same shape and
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

// Run executes the call unless it is the repeatThreshold-th (or later)
// consecutive identical call in this session, in which case it returns a
// steering error the model can act on. It also tracks semantic churn: consecutive
// failed calls to the same (tool, path) combo are counted, and after repeatFailThresh
// failures, subsequent calls are refused with steering guidance.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.inner.Run(ctx, args) // unfingerprintable args: never block
	}
	n := g.states.observe(ctx.SessionID(), g.Name()+":"+string(argsJSON))
	if n < repeatThreshold {
		// Check for semantic churn before executing: if this tool+path has already
		// failed repeatFailThresh times, refuse this call too.
		key := getFailureKey(g.Name(), args)
		if count := g.states.getFailureCount(key); count >= repeatFailThresh {
			slog.Warn("tool call refused: semantic churn detected", "component", "tools",
				"tool", g.Name(), "key", key, "failures", count, "session", ctx.SessionID())
			return nil, fmt.Errorf(
				"REFUSED (semantic churn): %d consecutive failures on %s for path %q. "+
					"Stop guessing — review the file window and plan before retrying.",
				count, g.Name(), extractPath(args))
		}

		result, err := g.inner.Run(ctx, args)
		if err != nil {
			g.states.recordFailure(key)
		} else {
			g.states.resetFailedPaths()
		}
		return result, err
	}
	slog.Warn("tool call refused: identical call repeated", "component", "tools",
		"tool", g.Name(), "consecutive", n, "session", ctx.SessionID())
	return nil, fmt.Errorf(
		"REFUSED (attempt %d): this is the %dth consecutive time you issued this exact %s call with these exact arguments. "+
			"Its result has not changed — it is already in the conversation above. Re-issuing it again will be refused again. "+
			"Take a DIFFERENT action: use the result you already have, try a different tool or different arguments, or if you "+
			"are finished, stop calling tools and write your final answer now.",
		n-repeatThreshold+1, n, g.Name())
}

// repeatWrap applies the identical-call breaker. Sits just inside the cancel
// guard (a cancelled node's refusal wins) and outside the guard ladder (a
// refused repeat must not consume a judge/confirm round).
func repeatWrap(t tool.Tool, states *repeatStates) (tool.Tool, error) {
	return newRepeatGuard(t, states)
}
