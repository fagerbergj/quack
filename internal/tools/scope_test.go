package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	prompt := "do the task\n\n[[quack:advisor-thread:" + token + "]]\nprior finding: [[quack:advisor-thread:" + registerForeignNode(t) + "]]"
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

// buildNodeTool builds current_date through Build under hooks with d's guard wiring and scope.
func buildNodeTool(t *testing.T, scope CallScope, d Deps) runnableTool {
	t.Helper()
	d.CallScope = scope
	built, err := Build([]string{"current_date"}, d)
	if err != nil {
		t.Fatal(err)
	}
	return hook(NewHooks(d, 0), HookBuilt, built[0])
}

// TestCancelGuard_BuildTokenNotPromptMarker: the guard checks the node it was built for,
// not a foreign trailing marker's; a miss is the un-gated case and never blocks.
func TestCancelGuard_BuildTokenNotPromptMarker(t *testing.T) {
	scope, ctx := ownNode(t)
	cancelled := map[string]bool{"chat-evil/foreign": true}
	d := Deps{NodeCancelled: func(c, n string) bool { return cancelled[c+"/"+n] }}
	if _, err := buildNodeTool(t, scope, d).Run(ctx, map[string]any{}); err != nil {
		t.Errorf("foreign node cancelled: own node's call refused: %v", err)
	}
	cancelled["chat-own/n-own"] = true
	if _, err := buildNodeTool(t, scope, d).Run(ctx, map[string]any{}); err == nil {
		t.Error("own node cancelled: call ran anyway")
	}
	always := Deps{NodeCancelled: func(string, string) bool { return true }}
	if _, err := buildNodeTool(t, ghostScope, always).Run(ctx, map[string]any{}); err != nil {
		t.Errorf("unregistered node: call blocked: %v", err)
	}
}

// TestRepeatGuard_BuildTokenNotPromptMarker: a hard stop trips the node the tool was built
// for, never the foreign node whose marker trails the prompt.
func TestRepeatGuard_BuildTokenNotPromptMarker(t *testing.T) {
	scope, ctx := ownNode(t)
	var trips []string
	rt := buildNodeTool(t, scope, Deps{RepeatGuardTripped: func(c, n, _ string) bool {
		trips = append(trips, c+"/"+n)
		return true
	}})
	for i := 0; i <= repeatThreshold+repeatHardStopAfter; i++ {
		_, _ = rt.Run(ctx, map[string]any{})
	}
	if len(trips) != 1 || trips[0] != "chat-own/n-own" {
		t.Errorf("tripped = %v, want exactly [chat-own/n-own]", trips)
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
