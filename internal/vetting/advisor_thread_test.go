package vetting

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestUnregisterMemSession_WarnsWhenNeverConnected pins #640's observability
// requirement: a session that was registered (the surface offered) but never
// saw a real request (MarkMemSessionConnected never called) must warn loudly
// on teardown - the silent "offered but unreachable" gap that let the #628
// rename survive a full day of dogfooding.
func TestUnregisterMemSession_WarnsWhenNeverConnected(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	secret := "test-secret-never-connected"
	RegisterMemSession(secret, MemSession{})
	UnregisterMemSession(secret)

	if !strings.Contains(buf.String(), "never connected") {
		t.Errorf("expected a warning about a never-connected session, got log: %s", buf.String())
	}
}

// TestUnregisterMemSession_SilentWhenConnected confirms the warning above
// doesn't fire on the normal, healthy path - MarkMemSessionConnected before
// teardown.
func TestUnregisterMemSession_SilentWhenConnected(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	secret := "test-secret-connected"
	RegisterMemSession(secret, MemSession{})
	MarkMemSessionConnected(secret)
	UnregisterMemSession(secret)

	if strings.Contains(buf.String(), "never connected") {
		t.Errorf("did not expect a never-connected warning, got log: %s", buf.String())
	}
}

// TestUnregisterAdvisorThread_FiresNodeSessionClosedHook pins the acp/vetting
// seam a pinned ACP process's cleanup rides on : every advisor
// thread teardown - not just the ones dag/graph.go happens to exercise - must
// reach NodeSessionClosed with the exact token, or a pinned subprocess for
// that node leaks forever with nothing left to evict it.
func TestUnregisterAdvisorThread_FiresNodeSessionClosedHook(t *testing.T) {
	old := NodeSessionClosed
	defer func() { NodeSessionClosed = old }()

	var got []string
	NodeSessionClosed = func(token string) { got = append(got, token) }

	token := "test-token-hook"
	RegisterAdvisorThread(token, AdvisorTask{})
	UnregisterAdvisorThread(token)

	if len(got) != 1 || got[0] != token {
		t.Fatalf("NodeSessionClosed calls = %v, want exactly one call with token %q", got, token)
	}
}

// TestUnregisterAdvisorThread_NilHookDoesNotPanic: acp wires NodeSessionClosed
// at server boot (serve.go); any other caller (an in-process test, `quack api`
// paths without a server, ...) must not crash for lack of that wiring.
func TestUnregisterAdvisorThread_NilHookDoesNotPanic(t *testing.T) {
	old := NodeSessionClosed
	defer func() { NodeSessionClosed = old }()
	NodeSessionClosed = nil

	token := "test-token-nil-hook"
	RegisterAdvisorThread(token, AdvisorTask{})
	UnregisterAdvisorThread(token) // must not panic
}

// TestUnregisterMemSession_BackstopDoubleCallDoesNotDoubleWarn pins the
// dag.buildGateNodes pattern: node.go's own explicit unregister plus a
// deferred backstop call both target the same secret. The second call must
// be a true no-op, not a second (and misleading, since it always finds the
// registry already cleared) never-connected warning.
func TestUnregisterMemSession_BackstopDoubleCallDoesNotDoubleWarn(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	secret := "test-secret-double-unregister"
	RegisterMemSession(secret, MemSession{})
	MarkMemSessionConnected(secret)
	UnregisterMemSession(secret) // the explicit call (node.go)
	UnregisterMemSession(secret) // the backstop defer (dag/graph.go)

	if strings.Contains(buf.String(), "never connected") {
		t.Errorf("backstop re-unregister produced a spurious never-connected warning: %s", buf.String())
	}
}
