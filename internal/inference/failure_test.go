package inference

import (
	"errors"
	"testing"
)

func TestRecordCallResult_TracksConsecutiveFailuresAndClearsOnSuccess(t *testing.T) {
	chatID, node := "chat-1105", "write-plan"
	t.Cleanup(func() { ClearFailure(chatID, node) })

	if _, _, _, ok := LastFailure(chatID, node); ok {
		t.Fatalf("expected no failure recorded before any call")
	}

	gwErr := errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`)
	for i := 0; i < 5; i++ {
		RecordCallResult(chatID, node, gwErr)
	}
	err, streak, _, ok := LastFailure(chatID, node)
	if !ok || streak != 5 || err.Error() != gwErr.Error() {
		t.Fatalf("LastFailure = (%v, %d, ok=%v), want the 502 error with streak 5", err, streak, ok)
	}

	// A later successful call clears the streak - a still-empty completion
	// after that is a genuine silent gap, not a masked gateway failure.
	RecordCallResult(chatID, node, nil)
	if _, _, _, ok := LastFailure(chatID, node); ok {
		t.Fatalf("expected failure state cleared after a successful call")
	}
}

func TestLastFailure_UnknownKeyReportsNotOK(t *testing.T) {
	if _, _, _, ok := LastFailure("no-such-chat", "no-such-node"); ok {
		t.Fatalf("expected ok=false for an untracked chat+node")
	}
}
