package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
)

// cmdWithServer sets a real context: Cobra leaves Context() nil until
// Execute runs, which a completer's context.Context param can't take.
func cmdWithServer(t *testing.T, url string) *cobra.Command {
	t.Helper()
	t.Setenv("QUACK_HOME", t.TempDir())
	c := &cobra.Command{}
	c.SetContext(context.Background())
	c.Flags().String("server", "", "")
	if err := c.Flags().Set("server", url); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCompleteChatIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"}]}`)
	}))
	defer srv.Close()

	got, directive := completeChatIDs(cmdWithServer(t, srv.URL), nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
	if len(got) != 1 || got[0] != "c1" {
		t.Errorf("completions = %v, want [c1]", got)
	}

	// A non-empty args (already past the id position) offers nothing.
	got, _ = completeChatIDs(cmdWithServer(t, srv.URL), []string{"c1"}, "")
	if got != nil {
		t.Errorf("completions with args already filled = %v, want none", got)
	}
}

// TestCompleteChatIDs_UnreachableServerTimesOut: a --server that never
// responds must not hang tab-complete - completionTimeout bounds the round
// trip. A handler that blocks past the deadline pins that bound
// deterministically, unlike relying on the OS to blackhole an unroutable IP.
func TestCompleteChatIDs_UnreachableServerTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	start := time.Now()
	got, directive := completeChatIDs(cmdWithServer(t, srv.URL), nil, "")
	elapsed := time.Since(start)
	if elapsed < completionTimeout {
		t.Errorf("completeChatIDs returned after %s, want it to wait out completionTimeout (%s), not return early", elapsed, completionTimeout)
	}
	if elapsed > 3*time.Second {
		t.Errorf("completeChatIDs took %s, want it bounded by completionTimeout (%s)", elapsed, completionTimeout)
	}
	if got != nil {
		t.Errorf("completions = %v, want none", got)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
}

func TestCompleteChatThenNodeIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/chats":
			io.WriteString(w, `{"data":[{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"}]}`)
		case r.URL.Path == "/api/v1/chats/c1":
			io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","turns":[{"id":"t1","created_at":"2026-01-01T00:00:00Z","input":{"role":"user","content":"hi"},"output":[{"type":"quack:dag","id":"d1","status":"completed","plan_id":"p1","nodes":[{"id":"n1","agent":"a","task":"t","depends_on":[]},{"id":"n2","agent":"a","task":"t","depends_on":["n1"]}],"edges":[],"node_states":{}}]}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	chats, _ := completeChatThenNodeIDs(cmdWithServer(t, srv.URL), nil, "")
	if len(chats) != 1 || chats[0] != "c1" {
		t.Errorf("chat-position completions = %v, want [c1]", chats)
	}

	nodes, _ := completeChatThenNodeIDs(cmdWithServer(t, srv.URL), []string{"c1"}, "")
	if len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Errorf("node-position completions = %v, want [n1 n2]", nodes)
	}

	none, directive := completeChatThenNodeIDs(cmdWithServer(t, srv.URL), []string{"c1", "n1"}, "")
	if none != nil || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completions past the node position = %v/%v, want none", none, directive)
	}
}

func TestCompleteMemoryIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"memories":[{"id":"m1","author":"a","bucket":"b","content":"c","kind":"k","status":"live","created_at":"2026-01-01T00:00:00Z"}],"total":1}`)
	}))
	defer srv.Close()

	got, _ := completeMemoryIDs(cmdWithServer(t, srv.URL), nil, "")
	if len(got) != 1 || got[0] != "m1" {
		t.Errorf("completions = %v, want [m1]", got)
	}
}

func TestCompleteServerNames(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	rc, err := cli.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.AddServer("a", "http://a"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Save(); err != nil {
		t.Fatal(err)
	}

	got, directive := completeServerNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("completions = %v, want [a]", got)
	}
}

func TestCompleteAgentNames(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "quack.yaml")
	cfg := `
providers:
  default:
    kind: openai
    endpoint: http://localhost:1
    api_key: x
orchestrator:
  provider: default
  model: m
models:
  m:
    provider: default
    role: worker
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: m
    acp:
      command: ["opencode", "acp"]
      read_only: true
stores:
  default:
    kind: sqlite
    url: ` + filepath.Join(dir, "store.db") + `
session:
  store: default
workspace:
  root: ` + filepath.Join(dir, "workspace") + `
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)

	got, _ := completeAgentNames(nil, nil, "")
	if len(got) != 1 || got[0] != "code-reviewer" {
		t.Errorf("completions = %v, want [code-reviewer]", got)
	}
}

// TestCompleteAgentNames_MissingOrMalformedConfig: the offline completer's
// invariant that matters most - never crash the shell - needs its own
// coverage: a missing QUACK_CONFIG path or unparseable YAML must degrade to
// no completions, not a panic.
func TestCompleteAgentNames_MissingOrMalformedConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) // sets QUACK_CONFIG for this case
	}{
		{"unset, no quack.yaml in cwd", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("QUACK_CONFIG", "")
		}},
		{"nonexistent path", func(t *testing.T) {
			t.Setenv("QUACK_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		}},
		{"malformed yaml", func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "quack.yaml")
			if err := os.WriteFile(p, []byte("not: [valid yaml"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("QUACK_CONFIG", p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			got, directive := completeAgentNames(nil, nil, "")
			if got != nil {
				t.Errorf("completions = %v, want none", got)
			}
			if directive != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("directive = %v, want NoFileComp", directive)
			}
		})
	}
}

// TestCompleteServerNames_EmptyRegistry: no servers registered must degrade
// to no completions, not a panic.
func TestCompleteServerNames_EmptyRegistry(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	got, directive := completeServerNames(nil, nil, "")
	if len(got) != 0 {
		t.Errorf("completions = %v, want none", got)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
}

func TestSandboxAgentFlagCompletionRegistered(t *testing.T) {
	for _, cmd := range []*cobra.Command{newSandboxCmd(), newSandboxRunCmd(), newSandboxCheckCmd(), newSandboxInfoCmd()} {
		if _, ok := cmd.GetFlagCompletionFunc("agent"); !ok {
			t.Errorf("%s: --agent has no completion func registered", cmd.Name())
		}
	}
}
