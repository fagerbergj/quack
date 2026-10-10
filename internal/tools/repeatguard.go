package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
)

// repeatThreshold: consecutive identical calls before the repeat guard refuses (1st=run, 2nd=retry, 3rd=refused).
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

// pathFailThreshold: consecutive failures against a (tool, resource) before refusing the next call.
const pathFailThreshold = 3

// crossCallTools: only the orchestrator's plan tools, where a same-args-same-result repeat is never progress
// (a worker re-reading a file is). Bounds crossCalls too: these are never in a worker's long-lived tool list.
// ponytail: load_skill isn't here, so an alternating load_skill/list_skills loop evades adjacency - add it if that shows up.
var crossCallTools = map[string]bool{"create_plan": true, "edit_plan": true, "execute": true}

// repeatKeys: one call's fingerprints; ok is false for unfingerprintable args, which are never blocked.
type repeatKeys struct {
	sessionID, fingerprint, crossKey, resource, resourceKey string
	hasResource, crossTracked                               bool
}

func newRepeatKeys(ctx agent.Context, name string, args map[string]any) (repeatKeys, bool) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return repeatKeys{}, false
	}
	k := repeatKeys{sessionID: ctx.SessionID(), fingerprint: name + ":" + string(argsJSON), crossTracked: crossCallTools[name]}
	k.crossKey = k.sessionID + "|" + k.fingerprint
	k.resource, k.hasResource = resourceFingerprint(name, argsJSON)
	k.resourceKey = name + ":" + k.resource
	return k, true
}

// checkRepeat refuses adjacent repeats, cross-call streaks and resource-failure churn; after repeatHardStopAfter
// more repeats, tripped ends a node's round and SkipSummarization the orchestrator's.
func (h *Hooks) checkRepeat(ctx agent.Context, name string, args map[string]any) error {
	k, ok := newRepeatKeys(ctx, name, args)
	if !ok {
		return nil
	}
	chatID, nodeID := h.scope.node(ctx)
	// Adjacency ignores the result: some tools' results always differ a little (a fresh revision number).
	if n := h.repeats.observe(k.sessionID, k.fingerprint); n >= repeatThreshold {
		if n > repeatThreshold+repeatHardStopAfter {
			return h.hardStop(ctx, name, k.sessionID, chatID, nodeID, n)
		}
		return refuseRepeat(name, k.sessionID, n, false)
	}
	// Cross-call: same args and same result with other calls between (e.g. edit_plan/execute alternating).
	if k.crossTracked && h.repeats.crossStreak(k.crossKey) >= repeatThreshold-1 {
		cn := h.repeats.recordCrossRefusal(k.crossKey)
		if cn > repeatThreshold+repeatHardStopAfter {
			return h.hardStop(ctx, name, k.sessionID, chatID, nodeID, cn)
		}
		return refuseRepeat(name, k.sessionID, cn, true) // crossStreak confirmed the result matches too
	}
	if !k.hasResource {
		return nil
	}
	if fails := h.repeats.resourceFailCount(k.sessionID, k.resourceKey); fails >= pathFailThreshold {
		slog.Warn("tool call refused: repeated failures against same resource", "component", "tools",
			"tool", name, "resource", k.resource, "consecutive_fails", fails, "session", k.sessionID)
		return &refusal{fmt.Sprintf(
			"REFUSED: %s against %q has now failed %d times in a row with varying arguments. Varying the arguments "+
				"further is not working - the problem is with %q itself or your understanding of it, not the "+
				"specific call. Stop retrying this resource: inspect it a different way (e.g. list its "+
				"surroundings), pick a different resource, or report the failure in your final answer.",
			name, k.resource, fails, k.resource)}
	}
	return nil
}

// recordRepeat folds a call that ran into the resource-failure and cross-call streaks.
func (h *Hooks) recordRepeat(ctx agent.Context, name string, args, result map[string]any, runErr error) {
	k, ok := newRepeatKeys(ctx, name, args)
	if !ok {
		return
	}
	if k.hasResource {
		h.repeats.observeResourceFail(k.sessionID, k.resourceKey, runErr != nil || batchAllFailed(name, result))
	}
	if k.crossTracked {
		h.repeats.observeCrossResult(k.crossKey, resultFingerprint(result, runErr))
	}
}

// refuseRepeat builds the soft-refusal error without ending the turn; n varies so consecutive refusals are
// never byte-identical. sameResult only for the cross-call tier: adjacency never compared results.
func refuseRepeat(name, sessionID string, n int, sameResult bool) error {
	slog.Warn("tool call refused: identical call repeated", "component", "tools",
		"tool", name, "consecutive", n, "session", sessionID)
	result := ""
	if sameResult {
		result = " and got the same result"
	}
	return &refusal{fmt.Sprintf(
		"REFUSED (attempt %d): this is the %s consecutive time you issued this exact %s call with these exact "+
			"arguments%s - it is already in the conversation above. Re-issuing it again will "+
			"END THIS TURN as a failure. Take a DIFFERENT action: use the result you already have, try a different "+
			"tool or different arguments, or if you are finished, stop calling tools and write your final answer now.",
		n-repeatThreshold+1, ordinal(n), name, result)}
}

// hardStop ends the turn once the model keeps re-issuing a refused call. It
// fires tripped; the orchestrator (nodeID=="") also gets SkipSummarization.
func (h *Hooks) hardStop(ctx agent.Context, name, sessionID, chatID, nodeID string, n int) error {
	msg := fmt.Sprintf("tool-call loop: %s called with identical arguments and the same result %d times despite being refused; turn terminated", name, n)
	slog.Warn("tool call loop: ending the turn", "component", "tools",
		"tool", name, "consecutive", n, "session", sessionID)
	if h.tripped != nil {
		h.tripped(chatID, nodeID, msg)
	}
	if nodeID == "" {
		ctx.Actions().SkipSummarization = true
	}
	h.repeats.resetSession(sessionID)
	return &refusal{msg}
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
