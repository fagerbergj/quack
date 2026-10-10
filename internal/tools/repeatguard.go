package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/ledger"
)

// repeatGuard: breaks identical-call loops - refuses the 3rd+ consecutive identical call.
type repeatGuard struct {
	runnableTool
	states *repeatStates
	// tripped fires on every hard stop, so a caller can tell one from a turn that produced nothing.
	tripped func(chatID, nodeID, msg string) bool
	scope   CallScope
}

// repeatThreshold: consecutive identical calls before refusal (1st=run, 2nd=retry, 3rd=refused).
const repeatThreshold = 3

// repeatHardStopAfter: identical calls allowed after a refusal before the turn is force-ended; ending
// on the first refusal denied the model any chance to correct its arguments within the turn.
const repeatHardStopAfter = 2

// repeatStates: per-session adjacency fingerprints plus per-(session, tool, args) result streaks (crossCalls).
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
	total    int // occurrences this session, refusals included: the refusal message's counter
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

// resetSession clears a session's adjacency and cross-call streaks after a hard stop, so a retry
// (e.g. a revise round) starts with a fresh budget instead of hard-stopping on its first call.
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

// crossStreak is checked before running, so a key at repeatThreshold-1 is refused rather than run
// again to reconfirm what two identical results already established.
func (s *repeatStates) crossStreak(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.crossCalls[key]; st != nil {
		return st.streak
	}
	return 0
}

// recordCrossRefusal bumps key's total but not its streak: a refused call has no result to fold in.
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

// observeCrossResult extends key's streak on an identical result and resets it otherwise, so a
// judge rejection whose reason varies never counts as a repeat.
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

func (s *repeatStates) resourceFailCount(sessionID, resourceKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails[sessionID+"|"+resourceKey]
}

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

func newRepeatGuard(inner tool.Tool, states *repeatStates, tripped func(chatID, nodeID, msg string) bool, scope CallScope) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support repeat guarding (not a runnable function tool)", inner.Name())
	}
	return &repeatGuard{runnableTool: rt, states: states, tripped: tripped, scope: scope}, nil
}

func (g *repeatGuard) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := g.runnableTool.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(c)
	}
}

func (g *repeatGuard) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return rebindToolMap(g.runnableTool, g, ctx, req)
}

// pathFailThreshold: consecutive failures against a (tool, resource) before refusing the next call.
const pathFailThreshold = 3

// crossCallTools: only the orchestrator's plan tools, where a same-args-same-result repeat is never progress
// (a worker re-reading a file is). Bounds crossCalls too: these are never in a worker's long-lived tool list.
var crossCallTools = map[string]bool{"create_plan": true, "edit_plan": true, "execute": true}

// Run refuses adjacent repeats, cross-call streaks and resource-failure churn; after repeatHardStopAfter more
// repeats, tripped ends a node's round and SkipSummarization the orchestrator's.
func (g *repeatGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return g.runnableTool.Run(ctx, args) // unfingerprintable args: never block
	}
	sessionID := ctx.SessionID()
	fingerprint := g.Name() + ":" + string(argsJSON)
	crossKey := sessionID + "|" + fingerprint
	chatID, nodeID := g.scope.node(ctx)
	crossTracked := crossCallTools[g.Name()]

	// Adjacency ignores the result: some tools' results always differ a little (a fresh revision number).
	if n := g.states.observe(sessionID, fingerprint); n >= repeatThreshold {
		if n > repeatThreshold+repeatHardStopAfter {
			return nil, g.hardStop(ctx, sessionID, chatID, nodeID, n)
		}
		return nil, g.refuse(sessionID, n, false)
	}
	// Cross-call: same args and same result with other calls between (e.g. edit_plan/execute alternating).
	if crossTracked && g.states.crossStreak(crossKey) >= repeatThreshold-1 {
		cn := g.states.recordCrossRefusal(crossKey)
		if cn > repeatThreshold+repeatHardStopAfter {
			return nil, g.hardStop(ctx, sessionID, chatID, nodeID, cn)
		}
		return nil, g.refuse(sessionID, cn, true) // crossStreak confirmed the result matches too
	}

	resource, hasResource := resourceFingerprint(g.Name(), argsJSON)
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

	result, runErr := g.runnableTool.Run(ctx, args)
	if hasResource {
		g.states.observeResourceFail(sessionID, resourceKey, runErr != nil || batchAllFailed(g.Name(), result))
	}
	if crossTracked {
		g.states.observeCrossResult(crossKey, resultFingerprint(result, runErr))
	}
	return result, runErr
}

// refuse builds the soft-refusal error without ending the turn; n varies so consecutive refusals are never
// byte-identical. sameResult only for the cross-call tier: adjacency never compared results.
func (g *repeatGuard) refuse(sessionID string, n int, sameResult bool) error {
	slog.Warn("tool call refused: identical call repeated", "component", "tools",
		"tool", g.Name(), "consecutive", n, "session", sessionID)
	result := ""
	if sameResult {
		result = " and got the same result"
	}
	return fmt.Errorf(
		"REFUSED (attempt %d): this is the %s consecutive time you issued this exact %s call with these exact "+
			"arguments%s - it is already in the conversation above. Re-issuing it again will "+
			"END THIS TURN as a failure. Take a DIFFERENT action: use the result you already have, try a different "+
			"tool or different arguments, or if you are finished, stop calling tools and write your final answer now.",
		n-repeatThreshold+1, ordinal(n), g.Name(), result)
}

// hardStop ends the turn once the model keeps re-issuing a refused call. It
// fires tripped; the orchestrator (nodeID=="") also gets SkipSummarization.
func (g *repeatGuard) hardStop(ctx agent.Context, sessionID, chatID, nodeID string, n int) error {
	msg := fmt.Sprintf("tool-call loop: %s called with identical arguments and the same result %d times despite being refused; turn terminated", g.Name(), n)
	slog.Warn("tool call loop: ending the turn", "component", "tools",
		"tool", g.Name(), "consecutive", n, "session", sessionID)
	if g.tripped != nil {
		g.tripped(chatID, nodeID, msg)
	}
	if nodeID == "" {
		ctx.Actions().SkipSummarization = true
	}
	g.states.resetSession(sessionID)
	return errors.New(msg)
}

// resourceFingerprint extracts the `path`/`url` field, or web_fetch's sorted
// `urls` batch, from tool args for failure-streak tracking.
func resourceFingerprint(toolName string, argsJSON []byte) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(argsJSON, &m); err != nil {
		return "", false
	}
	if toolName == "web_fetch" {
		if urls, ok := batchURLFingerprint(m); ok {
			return urls, true
		}
	}
	for _, key := range []string{"path", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// batchURLFingerprint: web_fetch's `urls` arg, sorted and joined - order-independent, so
// the same batch retried with its URLs reshuffled still fingerprints identically.
func batchURLFingerprint(args map[string]any) (string, bool) {
	urls, ok := args["urls"].([]any)
	if !ok {
		return "", false
	}
	strs := make([]string, 0, len(urls))
	for _, u := range urls {
		if s, ok := u.(string); ok && s != "" {
			strs = append(strs, s)
		}
	}
	if len(strs) == 0 {
		return "", false
	}
	slices.Sort(strs)
	return strings.Join(strs, ","), true
}

// batchAllFailed reports a batched tool call that ran but returned no
// successful entries (web_fetch's per-URL errors never surface as runErr).
func batchAllFailed(toolName string, result map[string]any) bool {
	if toolName != "web_fetch" {
		return false
	}
	results, ok := result["results"].([]any)
	if !ok || len(results) == 0 {
		return false
	}
	for _, r := range results {
		m, ok := r.(map[string]any)
		if !ok {
			return false
		}
		if _, failed := m["error"]; !failed {
			return false
		}
	}
	return true
}

// resultFingerprint lets a repeat count only when the result is unchanged too, not when it keeps
// changing (e.g. a judge whose rejection reason varies).

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
func repeatWrap(t tool.Tool, states *repeatStates, tripped func(chatID, nodeID, msg string) bool, scope CallScope) (tool.Tool, error) {
	return newRepeatGuard(t, states, tripped, scope)
}

// NewRepeatStates and RepeatWrap serve tools built outside Build, e.g. the orchestrator's plan tools.
func NewRepeatStates() *repeatStates { return newRepeatStates() }

// RepeatWrap: tripped fires on a hard stop (see repeatGuard.tripped); nil ignores it.
func RepeatWrap(t tool.Tool, states *repeatStates, tripped func(chatID, nodeID, msg string) bool) (tool.Tool, error) {
	return repeatWrap(t, states, tripped, CallScope{})
}

// SupportsRepeatGuard is false for a tool with no Run (e.g. memory.NewPreload()); callers wrapping a mixed
// list should leave such a tool as-is rather than treat RepeatWrap's error as fatal.
func SupportsRepeatGuard(t tool.Tool) bool {
	_, ok := t.(runnableTool)
	return ok
}

// repeatGuardedToolset applies repeatGuard to every tool a Toolset exposes -
// covers a toolset attached directly to the agent, which Build's per-tool wrapper chain never sees.
type repeatGuardedToolset struct {
	inner   tool.Toolset
	states  *repeatStates
	tripped func(chatID, nodeID, msg string) bool
	scope   CallScope
}

// RepeatWrapToolset shares states/tripped with the caller's other guarded tools, so ts's calls spend
// the same budget. scope is the node's (zero outside one).
func RepeatWrapToolset(ts tool.Toolset, states *repeatStates, tripped func(chatID, nodeID, msg string) bool, scope CallScope) tool.Toolset {
	return &repeatGuardedToolset{inner: ts, states: states, tripped: tripped, scope: scope}
}

func (w *repeatGuardedToolset) Name() string { return w.inner.Name() }

// Tools wraps each inner tool with repeatGuard; a non-runnable one passes through unwrapped.
// ponytail: load_skill isn't in crossCallTools, so an alternating load_skill/list_skills loop evades adjacency - add it there if that shape shows up.
func (w *repeatGuardedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	inner, err := w.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, len(inner))
	for i, t := range inner {
		if !SupportsRepeatGuard(t) {
			out[i] = t
			continue
		}
		if out[i], err = repeatWrap(t, w.states, w.tripped, w.scope); err != nil {
			return nil, err
		}
		// Toolset tools skip Build's registry, so emit here; zero coords: the round's ctx carries them.
		out[i] = emitWrap(out[i], ledger.Coords{})
	}
	return out, nil
}

// toolsetRequestProcessor structurally mirrors ADK's unexported toolinternal.RequestProcessor,
// which SkillToolset implements to inject its skill list into the system instruction.
type toolsetRequestProcessor interface {
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

func (w *repeatGuardedToolset) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if rp, ok := w.inner.(toolsetRequestProcessor); ok {
		return rp.ProcessRequest(ctx, req)
	}
	return nil
}
