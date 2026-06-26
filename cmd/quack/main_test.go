package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCommandTree asserts the cobra wiring: the expected verbs are registered
// (so a typo in AddCommand fails here, not at runtime) and `version` actually
// prints the stamp rather than erroring like the not-yet-wired stubs.
func TestCommandTree(t *testing.T) {
	root := newRootCmd()

	want := map[string][]string{
		"chat":   {"new", "resume", "list", "delete", "export", "stop", "node"},
		"server": {"run", "start", "stop", "status", "init", "use", "add", "list", "remove"},
		"api":    nil,
	}
	// Top-level `init` is the onboarding entry (local/remote branch).
	if c, _, err := root.Find([]string{"init"}); err != nil || c.Name() != "init" {
		t.Errorf("top-level init not registered: %v", err)
	}
	for name, subs := range want {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("command %q not registered: %v", name, err)
		}
		for _, sub := range subs {
			if c, _, err := root.Find([]string{name, sub}); err != nil || c.Name() != sub {
				t.Errorf("%s %s not registered: %v", name, sub, err)
			}
		}
	}

	// `chat node steer` is the deepest leaf — prove 3-level nesting resolves.
	if c, _, err := root.Find([]string{"chat", "node", "steer"}); err != nil || c.Name() != "steer" {
		t.Errorf("chat node steer not registered: %v", err)
	}

	// version prints the stamp and does not error.
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version command errored: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Errorf("version printed %q, want %q", got, version)
	}
}
