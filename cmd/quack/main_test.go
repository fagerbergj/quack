package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// TestUnknownSubcommandErrors: a typo under a group command fails (exit 1) with a suggestion instead of
// printing help and exiting 0, which a script would read as success; a bare group still prints help.
func TestUnknownSubcommandErrors(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		wantErr     string
		wantSuggest string
	}{
		{[]string{"chat", "lst"}, `unknown command "lst" for "quack chat"`, "list"},
		{[]string{"chat", "node", "pase"}, `unknown command "pase" for "quack chat node"`, "pause"},
		{[]string{"chat"}, "", ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(io.Discard)
			root.SetArgs(tc.args)
			err := root.Execute()
			if tc.wantErr == "" {
				if err != nil || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("bare group: err = %v, out = %q; want help and no error", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "Did you mean this?\n\t"+tc.wantSuggest) {
				t.Fatalf("err = %v, want %q suggesting %q", err, tc.wantErr, tc.wantSuggest)
			}
		})
	}
}

// TestChatListStatusHelpMatchesValidation: the --status help lists exactly the values validation accepts.
func TestChatListStatusHelpMatchesValidation(t *testing.T) {
	c, _, err := newRootCmd().Find([]string{"chat", "list"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Flags().Lookup("status").Usage, "filter by status: idle, running, needs_input, failed"; got != want {
		t.Errorf("--status help = %q, want %q", got, want)
	}
}

// TestSharedDBNotice: only a postgres session store (one a running server may share) earns the in-process warning.
func TestSharedDBNotice(t *testing.T) {
	for kind, want := range map[string]bool{"postgres": true, "": true, "sqlite": false} {
		cfg := &config.Config{Session: config.SessionConfig{Store: "s"}, Stores: map[string]config.StoreConfig{"s": {Kind: kind}}}
		if got := sharedDBNotice(cfg, "quack.yaml") != ""; got != want {
			t.Errorf("kind %q: notice = %v, want %v", kind, got, want)
		}
	}
}

// TestLocalStoreCommandsRejectServer: ledger show/rebuild/recover read the local stores, so a --server they
// would silently ignore is refused with the command to use instead.
func TestLocalStoreCommandsRejectServer(t *testing.T) {
	t.Setenv("QUACK_CONFIG", filepath.Join(t.TempDir(), "quack.yaml"))
	for _, args := range [][]string{{"ledger", "show", "c1"}, {"ledger", "rebuild", "c1"}, {"ledger", "recover"}} {
		root := newRootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append(args, "--server", "http://localhost:8080"))
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--server does not apply") || !strings.Contains(err.Error(), "quack ledger export") {
			t.Errorf("%v: err = %v, want --server refused with the export alternative", args, err)
		}
	}
}

// TestRecordingSource: judge replay and eval fetch a chat id's recording from --from-server, else --server.
func TestRecordingSource(t *testing.T) {
	c := cmdWithServer(t, "http://global")
	if got := recordingSource(c, ""); got != "http://global" {
		t.Errorf("no --from-server: got %q, want the global --server", got)
	}
	if got := recordingSource(c, "http://from"); got != "http://from" {
		t.Errorf("--from-server set: got %q, want it to win", got)
	}
}

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
