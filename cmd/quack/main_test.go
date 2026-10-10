package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCommandTree: the expected verbs are registered (an AddCommand typo fails here)
// and `version` prints the stamp.
func TestCommandTree(t *testing.T) {
	root := newRootCmd()

	want := map[string][]string{
		"chat":   {"new", "send", "show", "list", "delete", "export", "stop", "node", "rename", "archive", "unarchive"},
		"server": {"run", "init", "use", "add", "list", "remove"},
		"api":    nil,
		"ledger": {"list", "export", "show", "rebuild", "recover"},
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

	// The `chat node` leaves are the deepest: prove 3-level nesting resolves
	// and the node status, queue and edit surface is wired.
	for _, sub := range []string{"stop", "pause", "resume", "queue", "queue-edit", "queue-remove", "edit", "retry"} {
		if c, _, err := root.Find([]string{"chat", "node", sub}); err != nil || c.Name() != sub {
			t.Errorf("chat node %s not registered: %v", sub, err)
		}
	}

	// `chat resume` was removed with the TUI (superseded by `chat show` + `chat send`).
	if c, _, _ := root.Find([]string{"chat", "resume"}); c != nil && c.Name() == "resume" {
		t.Error("chat resume should not be registered - the TUI (and its resume verb) is gone")
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

// TestBareCommandPrintsHelp: bare `quack` prints root help (pointing at -p / chat send / chat show)
// without erroring.
func TestBareCommandPrintsHelp(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(nil)
	if err := root.Execute(); err != nil {
		t.Fatalf("bare quack errored: %v", err)
	}
	s := out.String()
	for _, want := range []string{"quack -p", "chat send", "chat show"} {
		if !strings.Contains(s, want) {
			t.Errorf("help output missing %q:\n%s", want, s)
		}
	}
}
