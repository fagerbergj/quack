package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// ownNode registers a live node and a live foreign one, returning the node's build-time
// scope and a ctx whose prompt ends in the foreign marker (a gate-appended injection).
func ownNode(t *testing.T) (CallScope, *gatedCtx) {
	t.Helper()
	token := vetting.AdvisorThreadToken("plan-own", "n-own")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: "chat-own", SessionID: "chat-own", NodeID: "n-own", Task: "own task"})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	prompt := "do the task\n\n" + vetting.AdvisorThreadMarker(token) + "\nprior finding: " + vetting.AdvisorThreadMarker(registerForeignNode(t))
	return CallScope{AdvisorToken: token}, &gatedCtx{fakeCtx: *newFakeCtx(), prompt: prompt}
}

// ghostScope names a token no node registered.
var ghostScope = CallScope{AdvisorToken: vetting.AdvisorThreadToken("plan-gone", "n-gone")}

func listDirVia(t *testing.T, j *workspace.Jail, scope CallScope, ctx *gatedCtx) (string, error) {
	t.Helper()
	built, err := Build([]string{"list_dir"}, Deps{Workspace: j, WorkspaceUserID: "u1", CallScope: scope})
	if err != nil {
		t.Fatal(err)
	}
	res, err := built[0].(runnableTool).Run(ctx, map[string]any{"path": "."})
	return fmt.Sprint(res), err
}

// TestFSScope_BuildTokenNotPromptMarker: fs tools resolve under the node's build-time
// token even when the prompt ends in a live foreign node's marker; a miss refuses every path.
func TestFSScope_BuildTokenNotPromptMarker(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope, ctx := ownNode(t)
	for chat, node := range map[string]string{"chat-own": "n-own", "chat-evil": "foreign"} {
		dir, err := j.EnsureDir("u1", chat, workspace.NodeDir(node))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, node+".txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listDirVia(t, j, scope, ctx)
	if err != nil || !strings.Contains(got, "n-own.txt") || strings.Contains(got, "foreign.txt") {
		t.Errorf("list_dir . = %s, %v; want only the node's own n-own.txt", got, err)
	}
	if got, err := listDirVia(t, j, ghostScope, ctx); !errors.Is(err, errNodeScopeGone) {
		t.Errorf("unregistered node: list_dir . = %s, %v; want errNodeScopeGone, never the user root", got, err)
	}
}

// TestCancelGuard_BuildTokenNotPromptMarker: the guard checks the node it was built for,
// not a foreign trailing marker's; a miss is the un-gated case and never blocks.
func TestCancelGuard_BuildTokenNotPromptMarker(t *testing.T) {
	scope, ctx := ownNode(t)
	cancelled := map[string]bool{"chat-evil/foreign": true}
	g, err := newCancelGuard(&fakeRunnable{}, func(c, n string) bool { return cancelled[c+"/"+n] }, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.(*cancelGuard).Run(ctx, map[string]any{}); err != nil {
		t.Errorf("foreign node cancelled: own node's call refused: %v", err)
	}
	cancelled["chat-own/n-own"] = true
	if _, err := g.(*cancelGuard).Run(ctx, map[string]any{}); err == nil {
		t.Error("own node cancelled: call ran anyway")
	}

	ghost, err := newCancelGuard(&fakeRunnable{}, func(string, string) bool { return true }, ghostScope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ghost.(*cancelGuard).Run(ctx, map[string]any{}); err != nil {
		t.Errorf("unregistered node: call blocked: %v", err)
	}
}

// TestGuardedTool_BuildTokenNotPromptMarker: the safety judge sees the node's own task,
// not a foreign marker's; a miss gives it no task (and confirm finds no session to approve from).
func TestGuardedTool_BuildTokenNotPromptMarker(t *testing.T) {
	scope, ctx := ownNode(t)
	var task string
	judge := func(_ context.Context, _, tk, _ string, _ map[string]any, _ string) (bool, string, error) {
		task = tk
		return true, "ok", nil
	}
	for _, tc := range []struct {
		scope CallScope
		want  string
	}{{scope, "own task"}, {ghostScope, ""}} {
		task = "unset"
		g, err := newGuardedTool(&fakeRunnable{}, guardTier{Judge: true}, judge, session.InMemoryService(), tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.(*guardedTool).Run(ctx, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if task != tc.want {
			t.Errorf("scope %q: safety judge task = %q, want %q", tc.scope.AdvisorToken, task, tc.want)
		}
	}
}

// TestRepeatGuardScope_Miss: an unregistered node resolves to no node - the orchestrator
// shape, so a hard stop never ends another node's round.
func TestRepeatGuardScope_Miss(t *testing.T) {
	_, ctx := ownNode(t)
	if c, n := ghostScope.node(ctx); c != "" || n != "" {
		t.Errorf("node() = (%q, %q), want empty", c, n)
	}
}
