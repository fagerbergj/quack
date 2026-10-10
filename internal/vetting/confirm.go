package vetting

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// GuardStatusKey / GuardResolvedKey: wire markers for guard-ladder confirm tier (internal/tools/guard.go).
const (
	GuardStatusKey = "status"
	// GuardResolvedKey marks a guarded tool's response as consuming a resolved confirm
	// decision, whether it ran on approval or refused on denial (see ConfirmDecision).
	GuardResolvedKey = "__quack_guard_resolved"
)

// confirmInterruptID: stable per-node, per-round confirm-pause key. Reuses the ask_user pause/resume path.
func confirmInterruptID(nodeID string, round int) string {
	return fmt.Sprintf("confirm-%s-r%d", nodeID, round)
}

// confirmTurn: one proposed-operation/decision exchange. hint is the guard's human-facing question.
type confirmTurn struct {
	tool   string
	args   map[string]any
	hint   string
	answer string
}

// confirmScanResult: full confirm history within one invocation, mirroring hitlScan.
type confirmScanResult struct {
	turns  []confirmTurn
	pauses int
}

// scanNodeConfirms: replays session events for guarded-tool confirm pauses.
func scanNodeConfirms(sess session.Session, invocationID, nodeID string) confirmScanResult {
	var s confirmScanResult
	pauses, answers := scanNodePauses(sess, invocationID, nodeID, "confirm", func(fc *genai.FunctionCall) {
		if fc.Name != toolconfirmation.FunctionCallName {
			return
		}
		turn := confirmTurn{tool: "(unknown)", hint: confirmationHint(fc.Args)}
		if oc, err := toolconfirmation.OriginalCallFrom(fc); err == nil {
			turn.tool, turn.args = oc.Name, oc.Args
		}
		s.turns = append(s.turns, turn)
	})
	s.pauses = pauses
	for i := range s.turns {
		s.turns[i].answer = answers[confirmInterruptID(nodeID, i+1)]
	}
	return s
}

// confirmationHint extracts the guard's hint from adk_request_confirmation args (handles live and persisted events).
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

// confirmApproved: lenient parse of human's confirm answer. Deny-by-default.
func confirmApproved(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "approve", "approved", "allow", "allowed", "confirm", "confirmed":
		return true
	default:
		return false
	}
}

// countGuardResolutions: how many guarded-tool responses consumed a confirm decision.
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

// ConfirmDecision: is this guarded-tool call the resolution of a just-answered confirm pause?
// Pinned to exact tool+args.
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

// sameArgs: JSON-normalized deep equality (handles int/float64 transport variance).
func sameArgs(a, b map[string]any) bool {
	na, aok := normalizeJSON(a)
	nb, bok := normalizeJSON(b)
	return aok && bok && reflect.DeepEqual(na, nb)
}

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

// withConfirmDecision: builds the post-decision prompt.
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
