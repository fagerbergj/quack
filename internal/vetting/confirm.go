package vetting

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
)

// GuardStatusKey / GuardResolvedKey are the wire marker vocabulary a
// guard-wrapped tool (internal/tools/guard.go, the guard ladder's confirm
// tier - see .quack/plan-pr5-tool-schemas.md §4b) uses in its
// FunctionResponse. The PENDING marker itself is ADK-NATIVE: a guarded tool
// awaiting confirmation calls agent.Context.RequestConfirmation, which makes
// the llm flow emit an `adk_request_confirmation` FunctionCall event
// (toolconfirmation.FunctionCallName, carrying the original call) -
// scanNodeConfirms watches for THAT, mirroring AskToolName's "the gate
// watches a session-event marker" design.
const (
	GuardStatusKey = "status"
	// GuardResolvedKey marks a guarded tool's response as the consumption of a
	// previously-resolved confirm decision (whether it executed for real on
	// approval, or returned a refusal on denial) - see ConfirmDecision.
	GuardResolvedKey = "__quack_guard_resolved"
)

// confirmInterruptID is the STABLE per-node, per-round interrupt key for a
// guard-ladder confirm pause - the confirm-tier sibling of hitlInterruptID,
// riding the SAME workflow.ResumeOrRequestInput/ctx.ResumedInput mechanism
// (see RunGatedRefine) so it reuses ask_user's proven node-level pause/resume
// path end to end (executor.go's `ev.RequestedInput != nil` handling and
// orchestrator's latestPendingNodeInterrupt need no confirm-specific code -
// only hitlIDRe's prefix pattern is widened to recognize "confirm-").
func confirmInterruptID(nodeID string, round int) string {
	return fmt.Sprintf("confirm-%s-r%d", nodeID, round)
}

// confirmTurn is one proposed-operation/decision exchange within a node's
// confirm history. answer is "" until the corresponding pause is resolved.
// hint is the guard's human-facing question (internal/tools/guard.go) - it
// carries e.g. the "this DIFFERS from the previously approved operation"
// warning, so the node's pause message must prefer it over a generic one.
type confirmTurn struct {
	tool   string
	args   map[string]any
	hint   string
	answer string
}

// confirmScanResult summarizes a node's FULL confirm history within ONE
// invocation, mirroring hitlScan: every guarded operation proposed so far
// (turns) and how many of those the gate has already paused for (pauses).
type confirmScanResult struct {
	turns  []confirmTurn
	pauses int
}

// scanNodeConfirms replays session events scoped to nodeID/invocationID,
// collecting every ADK-native `adk_request_confirmation` FunctionCall the
// llm flow emitted for a guarded tool under this node (the original call's
// name/args come from the wrapper call via toolconfirmation.OriginalCallFrom),
// plus how many confirm pauses the gate already raised, and folding in any
// already-delivered decisions. Mirrors scanNodeAsks.
func scanNodeConfirms(sess session.Session, invocationID, nodeID string) confirmScanResult {
	var s confirmScanResult
	if sess == nil {
		return s
	}
	prefix := "confirm-" + nodeID + "-r"
	answers := map[string]string{} // interruptID → the human's decision text

	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID {
			continue
		}
		if ev.Author == "user" {
			for _, p := range ev.Content.Parts {
				if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != workflow.WorkflowInputFunctionCallName {
					continue
				}
				if !strings.HasPrefix(p.FunctionResponse.ID, prefix) {
					continue
				}
				if payload, ok := p.FunctionResponse.Response["payload"].(string); ok {
					answers[p.FunctionResponse.ID] = payload
				}
			}
			continue
		}
		if !pathHasNode(ev, nodeID) {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			switch p.FunctionCall.Name {
			case workflow.WorkflowInputFunctionCallName:
				if strings.HasPrefix(p.FunctionCall.ID, prefix) {
					s.pauses++
				}
			case toolconfirmation.FunctionCallName:
				turn := confirmTurn{tool: "(unknown)"}
				if oc, err := toolconfirmation.OriginalCallFrom(p.FunctionCall); err == nil {
					turn.tool, turn.args = oc.Name, oc.Args
				}
				turn.hint = confirmationHint(p.FunctionCall.Args)
				s.turns = append(s.turns, turn)
			}
		}
	}
	for i := range s.turns {
		s.turns[i].answer = answers[confirmInterruptID(nodeID, i+1)]
	}
	return s
}

// confirmationHint extracts the guard's human-facing hint from an
// adk_request_confirmation call's args. The "toolConfirmation" value is the
// in-memory struct on a live event, or a plain JSON map after a persistence
// round trip - handle both.
func confirmationHint(args map[string]any) string {
	switch tc := args["toolConfirmation"].(type) {
	case toolconfirmation.ToolConfirmation:
		return tc.Hint
	case *toolconfirmation.ToolConfirmation:
		if tc != nil {
			return tc.Hint
		}
	case map[string]any:
		if h, ok := tc["hint"].(string); ok {
			return h
		}
	}
	return ""
}

// confirmApproved parses a human's free-text confirm answer leniently.
// Anything unrecognized (including empty) is a denial - deny-by-default is
// the safe direction for an unparseable answer.
func confirmApproved(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "approve", "approved", "allow", "allowed", "confirm", "confirmed":
		return true
	default:
		return false
	}
}

// countGuardResolutions counts how many of this node's guarded-tool responses
// already consumed a confirm decision (GuardResolvedKey) - see ConfirmDecision.
func countGuardResolutions(sess session.Session, invocationID, nodeID string) int {
	if sess == nil {
		return 0
	}
	n := 0
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID || !pathHasNode(ev, nodeID) {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.FunctionResponse == nil {
				continue
			}
			if v, ok := p.FunctionResponse.Response[GuardResolvedKey].(bool); ok && v {
				n++
			}
		}
	}
	return n
}

// ConfirmDecision reports whether the CURRENT guarded-tool call (from
// internal/tools/guard.go) is the resolution of a just-answered confirm
// pause. A decision is PINNED to the exact operation the human saw: matched
// is true only when an unconsumed resolved decision exists for this node
// whose pinned tool name AND arguments (JSON-normalized deep equality - see
// sameArgs) equal the incoming call's, in which case approved carries that
// decision. A call under the same tool name but with DIFFERENT arguments
// never consumes the approval (the human approved a specific operation, not
// a blank check on the tool): matched is false and mismatched is true, so
// the guard wrapper treats it as a brand-new proposal - fresh judge tier,
// fresh confirmation whose hint notes the difference - while the original
// approval stays available for a subsequent same-args call. Stateless:
// everything is re-derived from session history each call, the same
// discipline scanNodeAsks/hitlScan already use for ask_user - no separate
// ledger, so it can never drift from what actually happened.
func ConfirmDecision(sess session.Session, invocationID, nodeID, toolName string, args map[string]any) (approved, matched, mismatched bool) {
	scan := scanNodeConfirms(sess, invocationID, nodeID)
	consumed := countGuardResolutions(sess, invocationID, nodeID)
	seen := 0
	for _, t := range scan.turns {
		if t.answer == "" {
			continue // still pending; not a decision
		}
		seen++
		if seen <= consumed {
			continue // already consumed by an earlier guarded-tool response
		}
		if t.tool == toolName && sameArgs(t.args, args) {
			return confirmApproved(t.answer), true, false
		}
		mismatched = true
	}
	return false, false, mismatched
}

// sameArgs compares two tool-argument maps by JSON-normalized deep equality:
// both sides are round-tripped through json.Marshal/Unmarshal so numeric
// representation differences from JSON transport (an int pinned at approval
// time vs the same value arriving as float64 on the re-issued call) can't
// break - or fake - a match. A side that fails to marshal never matches.
func sameArgs(a, b map[string]any) bool {
	na, aok := normalizeJSON(a)
	nb, bok := normalizeJSON(b)
	return aok && bok && reflect.DeepEqual(na, nb)
}

// normalizeJSON canonicalizes m via a marshal/unmarshal round trip (all
// numbers become float64, nested types collapse to plain JSON shapes, and a
// nil map unifies with an empty one).
func normalizeJSON(m map[string]any) (any, bool) {
	if m == nil {
		m = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false
	}
	return v, true
}

// withConfirmDecision builds the self-contained prompt for the post-decision
// worker run, folding in the operation that was pending and the human's
// decision - mirrors withUserAnswer's "fresh prompt, not raw protocol replay"
// idiom. On approval the worker is told to re-issue the SAME call now; the
// guard wrapper recognizes that immediately-following call as the approved
// execution (see internal/tools/guard.go's ConfirmDecision use).
func withConfirmDecision(prompt string, turns []confirmTurn) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n--- You proposed an operation that required human confirmation ---\n")
	for _, t := range turns {
		if t.answer == "" {
			continue // not yet resolved; shouldn't happen for a round we're folding in
		}
		decision := "DENIED - do not attempt this operation again; continue the task another way"
		if confirmApproved(t.answer) {
			decision = "APPROVED - call it again now, with the same arguments, to execute it"
		}
		fmt.Fprintf(&b, "Proposed: %s(%v)\nDecision: %s\n", t.tool, t.args, decision)
	}
	b.WriteString("\nContinue the task now using this decision.")
	return b.String()
}
