package inference

import (
	"errors"
	"strings"
	"testing"
)

func TestRecordCallResult_TracksConsecutiveFailuresAndClearsOnSuccess(t *testing.T) {
	chatID, node, agent := "chat-1105", "write-plan", "synthesizer"
	t.Cleanup(func() { ClearFailure(chatID, node, agent) })

	if _, _, _, ok := LastFailure(chatID, node, agent); ok {
		t.Fatalf("expected no failure recorded before any call")
	}

	gwErr := errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`)
	for i := 0; i < 5; i++ {
		RecordCallResult(chatID, node, agent, gwErr)
	}
	err, streak, _, ok := LastFailure(chatID, node, agent)
	if !ok || streak != 5 || err.Error() != gwErr.Error() {
		t.Fatalf("LastFailure = (%v, %d, ok=%v), want the 502 error with streak 5", err, streak, ok)
	}

	// A later successful call clears the streak - a still-empty completion
	// after that is a genuine silent gap, not a masked gateway failure.
	RecordCallResult(chatID, node, agent, nil)
	if _, _, _, ok := LastFailure(chatID, node, agent); ok {
		t.Fatalf("expected failure state cleared after a successful call")
	}
}

func TestLastFailure_UnknownKeyReportsNotOK(t *testing.T) {
	if _, _, _, ok := LastFailure("no-such-chat", "no-such-node", "no-such-agent"); ok {
		t.Fatalf("expected ok=false for an untracked chat+node+agent")
	}
}

// TestRecordCallResult_KeysByAgentRole is #1109 review finding 3: a judge
// failure on the same chat+node must not be visible under the worker's own
// agent role, or a later unrelated empty completion from a healthy model
// would misreport the judge's failure as its own gateway error.
func TestRecordCallResult_KeysByAgentRole(t *testing.T) {
	const chatID, node = "chat-1105-roles", "write-plan"
	t.Cleanup(func() {
		ClearFailure(chatID, node, "judge")
		ClearFailure(chatID, node, "synthesizer")
	})

	RecordCallResult(chatID, node, "judge", errors.New("judge boom"))
	if _, _, _, ok := LastFailure(chatID, node, "synthesizer"); ok {
		t.Fatalf("a judge failure must not be visible under the worker's own agent role")
	}
	if _, streak, _, ok := LastFailure(chatID, node, "judge"); !ok || streak != 1 {
		t.Fatalf("judge failure not recorded under its own role: streak=%d ok=%v", streak, ok)
	}
}

func TestSanitizeGatewayError_DropsURLAndBodyKeepsStatusClass(t *testing.T) {
	raw := errors.New(`openai qwen3.8-27b (generate): status 401: POST "http://llm-swap:11436/v1/chat/completions": {"error":"Incorrect API key provided: sk-super-secret-123"}`)
	summary, transient := SanitizeGatewayError(raw)
	if transient {
		t.Errorf("401 must not be classified transient")
	}
	if summary != "model gateway returned 401 Unauthorized" {
		t.Fatalf("summary = %q, want a status-only classification", summary)
	}
	for _, leaked := range []string{"sk-super-secret-123", "llm-swap", "11436", "POST"} {
		if strings.Contains(summary, leaked) {
			t.Fatalf("summary = %q leaked %q", summary, leaked)
		}
	}
}

func TestSanitizeGatewayError_5xxIsTransient(t *testing.T) {
	raw := errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`)
	summary, transient := SanitizeGatewayError(raw)
	if !transient {
		t.Errorf("502 must be classified transient")
	}
	if !TransientFromSummary(summary) {
		t.Errorf("TransientFromSummary(%q) = false, want true", summary)
	}
}
