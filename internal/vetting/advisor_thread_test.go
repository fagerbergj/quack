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
