package vetting

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A session registered but never connected must warn on teardown: an offered-but-unreachable
// surface is otherwise silent.
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

// Every teardown must reach NodeSessionClosed with the exact token, or a pinned ACP
// subprocess for that node leaks with nothing left to evict it.
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

// NodeSessionClosed is wired only at server boot; other callers must not crash without it.
func TestUnregisterAdvisorThread_NilHookDoesNotPanic(t *testing.T) {
	old := NodeSessionClosed
	defer func() { NodeSessionClosed = old }()
	NodeSessionClosed = nil

	token := "test-token-nil-hook"
	RegisterAdvisorThread(token, AdvisorTask{})
	UnregisterAdvisorThread(token) // must not panic
}

// An explicit unregister plus a deferred backstop hit the same secret; the second call must
// be a no-op, not a second never-connected warning.
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
