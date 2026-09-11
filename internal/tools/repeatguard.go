package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// repeatGuard: breaks identical-call loops - refuses the 3rd+ consecutive identical call.
type repeatGuard struct {
	inner  runnableTool
	states *repeatStates
	// tripped reaches dag.Executor.RepeatGuardTripped to end a WORKER NODE's
	// round on a hard stop - a returned tool error alone can't: ADK folds it
	// into a function response and keeps the model's turn going. nil for a
	// tool that's never node-scoped (nodeScope(ctx) then always resolves
	// nodeID=="") - see Run()'s orchestrator branch, which ends the
	// ORCHESTRATOR's own turn directly instead, without needing a
	// caller-supplied callback for it.
	tripped func(chatID, nodeID, msg string) bool
}

// repeatThreshold: consecutive identical calls before refusal (1st=run, 2nd=retry, 3rd=refused).
const repeatThreshold = 3

// repeatHardStopAfter: further identical calls a WORKER NODE's model can
// make after being refused before its round is force-ended via tripped. The
// orchestrator has no such grace period - its own turn already ends at the
// first refusal (Run()'s orchestrator branch), so it never reaches this
// tier in the same turn.
const repeatHardStopAfter = 2

// repeatStates: tracks last call fingerprint per session (adjacency), plus
// each (session, tool, args) key's own cross-call streak (crossCalls),
// unaffected by call order - so the same tool called with the same args
// counts toward ITS OWN streak even with other tool calls (or other args to
// the same tool) sitting between, which is exactly the shape a model stuck
// alternating two different tools takes (e.g. edit_plan/execute). Both
// checks run independently in Run(): adjacency catches a tool whose result
// always differs a little even on a genuine immediate repeat (e.g.
// write_artifact's own fresh revision number); crossCalls catches the same
// tool+args recurring with the SAME result despite other calls between
// occurrences, without flagging a result that keeps changing (e.g.
// execute's own per-round judge feedback).
// ponytail: entries never pruned - add if sessions number in the millions.
type repeatStates struct {
	mu         sync.Mutex
	last       map[string]*repeatState
	crossCalls map[string]*crossCallState
	fails      map[string]int
}

type repeatState struct {
	fingerprint string
	count       int
}

type crossCallState struct {
	resultFP string
	streak   int // consecutive occurrences of this key whose result matched the previous one
	total    int // total occurrences of this key this session, refusals included - drives the refusal message's counter, so it never repeats itself
}

func newRepeatStates() *repeatStates {
	return &repeatStates{last: map[string]*repeatState{}, crossCalls: map[string]*crossCallState{}, fails: map[string]int{}}
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

// resetSession clears a session's streak counter after a hard stop, so a
// retry (e.g. a revise round) starts with a fresh budget instead of
// immediately hard-stopping again on its very first call. Also drops this
// session's crossCalls entries (keyed sessionID+"|"+...) - a hard stop means
// the same key already built a cross streak too, and leaving it would trip
// the cross check on the very next identical call instead of the fresh
// budget this is meant to grant.
func (s *repeatStates) resetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.last, sessionID)
	prefix := sessionID + "|"
	for k := range s.crossCalls {
		if strings.HasPrefix(k, prefix) {
			delete(s.crossCalls, k)
		}
	}
}

// crossStreak returns key's current confirmed-identical-result streak
// without mutating it - checked BEFORE running, so a key already at
// repeatThreshold-1 is refused on this call rather than run a further time
// just to reconfirm what two prior identical results already established.
func (s *repeatStates) crossStreak(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.crossCalls[key]; st != nil {
		return st.streak
	}
	return 0
}

// recordCrossRefusal bumps key's total for the refusal message's counter,
// without touching streak - a refused call never ran, so there's no new
// result to fold in.
func (s *repeatStates) recordCrossRefusal(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.crossCalls[key]
	if st == nil {
		st = &crossCallState{}
		s.crossCalls[key] = st
	}
	st.total++
	return st.total
}

// observeCrossResult folds a completed call's own outcome into key's
// streak: a result identical to the last one recorded for this key extends
// the streak; a different one resets it to 1 - a judge rejection whose
// reason varies round to round must never count as a repeat just because
// the call arguments happened to stay the same.
func (s *repeatStates) observeCrossResult(key, resultFP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.crossCalls[key]
	if st == nil {
		s.crossCalls[key] = &crossCallState{resultFP: resultFP, streak: 1, total: 1}
		return
	}
	st.total++
	if st.resultFP == resultFP {
		st.streak++
	} else {
		st.resultFP = resultFP
		st.streak = 1
	}
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

func newRepeatGuard(inner tool.Tool, states *repeatStates, tripped func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support repeat guarding (not a runnable function tool)", inner.Name())
	}
	return &repeatGuard{inner: rt, states: states, tripped: tripped}, nil
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

// Run: refuses on an immediate back-to-back identical call, a same-tool
// identical-args-identical-result streak persisting across other calls
// between occurrences, or resource-failure churn; once the model repeats a
// refused call repeatHardStopAfter more times, ends a worker node's round.
// A call never scoped to a worker node (nodeScope resolves nodeID=="") is
// the orchestrator's own - its turn ends on the first refusal instead of
// waiting for the hard-stop tier, since it has no gate/continuation loop of
// its own to keep it going.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.inner.Run(ctx, args) // unfingerprintable args: never block
	}
	sessionID := ctx.SessionID()
	fingerprint := g.Name() + ":" + string(argsJSON)
	crossKey := sessionID + "|" + fingerprint
	chatID, nodeID := nodeScope(ctx)

	// Adjacency: this exact call immediately repeated, regardless of
	// result - catches a tool whose own result always differs a little
	// even on a genuine immediate repeat (e.g. a fresh artifact revision
	// number on an otherwise byte-identical write).
	n := g.states.observe(sessionID, fingerprint)
	if n > repeatThreshold+repeatHardStopAfter {
		msg := fmt.Sprintf("tool-call loop: %s called with identical arguments %d consecutive times despite being refused; node terminated", g.Name(), n)
		slog.Warn("tool call loop: ending node turn", "component", "tools",
			"tool", g.Name(), "consecutive", n, "session", sessionID)
		if nodeID != "" && g.tripped != nil {
			g.tripped(chatID, nodeID, msg)
		}
		g.endTurnIfOrchestrator(ctx, nodeID)
		g.states.resetSession(sessionID) // a retry (e.g. revise) starts with a fresh budget, not an already-blown one
		return nil, errors.New(msg)
	}
	if n >= repeatThreshold {
		return nil, g.refuse(ctx, sessionID, nodeID, n)
	}
	// Cross-call: same tool+args recurring with the SAME result even with
	// other tool calls (or other args to this tool) sitting between - the
	// shape a model stuck alternating two tools takes (e.g.
	// edit_plan/execute). Never trips on a result that keeps changing
	// (e.g. execute's own per-round judge feedback).
	if g.states.crossStreak(crossKey) >= repeatThreshold-1 {
		cn := g.states.recordCrossRefusal(crossKey)
		return nil, g.refuse(ctx, sessionID, nodeID, cn)
	}

	resource, hasResource := resourceFingerprint(argsJSON)
	resourceKey := g.Name() + ":" + resource
	if hasResource {
		if fails := g.states.resourceFailCount(sessionID, resourceKey); fails >= pathFailThreshold {
			slog.Warn("tool call refused: repeated failures against same resource", "component", "tools",
				"tool", g.Name(), "resource", resource, "consecutive_fails", fails, "session", sessionID)
			g.endTurnIfOrchestrator(ctx, nodeID)
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
	g.states.observeCrossResult(crossKey, resultFingerprint(result, runErr))
	return result, runErr
}

// refuse logs, ends the orchestrator's own turn when this call isn't
// node-scoped, and builds the REFUSED error both the adjacency and
// cross-call checks return - n is whichever check's own occurrence count
// tripped, so consecutive refusals for the same key are never
// byte-identical (the model can tell it's still being refused, not stuck on
// a cached response).
func (g *repeatGuard) refuse(ctx agent.Context, sessionID, nodeID string, n int) error {
	slog.Warn("tool call refused: identical call repeated", "component", "tools",
		"tool", g.Name(), "consecutive", n, "session", sessionID)
	g.endTurnIfOrchestrator(ctx, nodeID)
	return fmt.Errorf(
		"REFUSED (attempt %d): this is the %s consecutive time you issued this exact %s call with these exact "+
			"arguments and got the same result - it is already in the conversation above. Re-issuing it again will "+
			"END THIS TURN as a failure. Take a DIFFERENT action: use the result you already have, try a different "+
			"tool or different arguments, or if you are finished, stop calling tools and write your final answer now.",
		n-repeatThreshold+1, ordinal(n), g.Name())
}

// endTurnIfOrchestrator ends the calling turn outright when this call isn't
// node-scoped (nodeID=="") - the orchestrator's own tools (create_plan/
// edit_plan/execute/list_nodes), which have no gate/continuation loop of
// their own to keep re-issuing the refused call, unlike a worker node's
// tools, which go through the tripped/hard-stop tier above instead.
func (g *repeatGuard) endTurnIfOrchestrator(ctx agent.Context, nodeID string) {
	if nodeID == "" {
		ctx.Actions().SkipSummarization = true
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

// resultFingerprint serializes a call's own outcome so repeatStates can tell
// "the exact same call, producing the exact same result" (a real loop) from
// "the exact same call whose result keeps changing" (e.g. a judge round
// whose rejection reason varies) - only the former counts as a repeat.
func resultFingerprint(result map[string]any, runErr error) string {
	if runErr != nil {
		return "err:" + runErr.Error()
	}
	b, err := json.Marshal(result)
	if err != nil {
		// Unfingerprintable result: always-different, so it can never look
		// like a false repeat (mirrors the args-marshal guard above).
		return fmt.Sprintf("unfingerprintable:%p", result)
	}
	return "ok:" + string(b)
}

// ordinal renders n in English ordinal form (1st, 2nd, 3rd, 4th, 11th, ...)
// for the refusal message - a plain "%dth" prints "3th".
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// repeatWrap applies the identical-call breaker.
func repeatWrap(t tool.Tool, states *repeatStates, tripped func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	return newRepeatGuard(t, states, tripped)
}

// NewRepeatStates and RepeatWrap expose the identical-call breaker to a
// caller outside this package that builds tools individually rather than
// through Build - the orchestrator's own hand-built list_nodes/create_plan/
// edit_plan/execute, which Build's registry loop never sees and so never
// wrapped (a model that keeps sending the same malformed create_plan call
// had nothing stopping it - see the QA rig's 386-call loop this closes).
func NewRepeatStates() *repeatStates { return newRepeatStates() }

// RepeatWrap is repeatWrap, exported for the same reason as NewRepeatStates.
func RepeatWrap(t tool.Tool, states *repeatStates) (tool.Tool, error) {
	return repeatWrap(t, states, nil)
}

// SupportsRepeatGuard reports whether t can be passed to RepeatWrap - false
// for a tool with no Run (e.g. memory.NewPreload(), which only mutates the
// request and is never independently "called" by the model, so it has
// nothing a repeated-call loop could target). A caller wrapping a
// heterogeneous list built outside Build's own registry (which only ever
// holds runnable function tools) should check this first and leave a
// non-runnable tool as-is, rather than treat RepeatWrap's error as fatal.
func SupportsRepeatGuard(t tool.Tool) bool {
	_, ok := t.(runnableTool)
	return ok
}
