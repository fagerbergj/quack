package inference

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
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

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// TestSanitizeStoreError_NeverLeaksDSNFragments covers the two exotic DSN
// shapes the #1200 review flagged: a single-quoted password containing an
// @, and a raw user@host embedded inside a DSN. Both are baked into the
// wrapped error's text (as a real pgconn/gorm error might carry), proving
// SanitizeStoreError's structured-field-only approach never echoes them -
// unlike the prior regex approach, which matched only up to the first @ or
// unquoted whitespace and leaked the remainder.
func TestSanitizeStoreError_NeverLeaksDSNFragments(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		leaked []string
	}{
		{
			name: "single-quoted password containing @",
			err: &net.OpError{Op: "dial", Net: "tcp", Addr: fakeAddr("quack-postgres:5432"),
				Err: fmt.Errorf("dsn host=quack-postgres password='p@ss' sslmode=disable: %w", syscall.ECONNREFUSED)},
			leaked: []string{"p@ss", "password="},
		},
		{
			name: "user@host embedded in DSN",
			err: &net.OpError{Op: "dial", Net: "tcp", Addr: fakeAddr("quack-postgres:5432"),
				Err: fmt.Errorf("postgres://user:pa@ss@quack-postgres:5432/db: %w", syscall.ECONNREFUSED)},
			leaked: []string{"pa@ss", "user:pa", "user@host"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeStoreError(tc.err)
			for _, frag := range tc.leaked {
				if strings.Contains(got, frag) {
					t.Fatalf("sanitized message %q leaked fragment %q", got, frag)
				}
			}
			if !strings.Contains(got, "quack-postgres:5432") {
				t.Fatalf("sanitized message %q missing expected host:port", got)
			}
			if !strings.Contains(got, "connection refused") {
				t.Fatalf("sanitized message %q missing expected error class", got)
			}
		})
	}
}

func TestSanitizeStoreError_NoSuchHost(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "quack-postgres", IsNotFound: true}
	got := SanitizeStoreError(err)
	if !strings.Contains(got, "quack-postgres") || !strings.Contains(got, "no such host") {
		t.Fatalf("sanitized message = %q, want host + no such host class", got)
	}
}

func TestSanitizeStoreError_NilIsEmpty(t *testing.T) {
	if got := SanitizeStoreError(nil); got != "" {
		t.Fatalf("SanitizeStoreError(nil) = %q, want empty", got)
	}
}
