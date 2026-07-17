package tools

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/workspace"
)

// A tool error must speak the ONE namespace the model speaks. This is the same
// invariant #204/#209/#217 established for RESULTS — a path out of any tool goes
// back into any tool — still leaking through the ERROR paths.
//
// Live (code mode's first run, 2026-07-13): the model asked for
// "internal/tools/registry.go" and was shown
//
//	read_file: stat /tmp/claude-1000/-home-jason-…/scratchpad/workspace/local/
//	2dfbfc35-7114-4065-84db-bab4b4abdb9e/explorer/internal/tools/registry.go:
//	no such file or directory
//
// — the workspace root, the CHAT id and the NODE id, none of which the model has
// ever seen or can ever type. It also hands the host's layout to the model.
//
// The ids below are deliberately distinctive strings: any of them appearing in a
// returned error is the leak.
const (
	leakChatID = "2dfbfc35-7114-4065-84db-bab4b4abdb9e"
	leakNodeID = "explorer"
)

// buildToolsForLeakTest builds the real, fully-wrapped tool set over a fresh jail
// (exactly as production does — same Build) and returns it with the jail root, so
// a test can grep a returned error for the host path.
func buildToolsForLeakTest(t *testing.T, names ...string) (map[string]tool.Tool, string) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	built, err := Build(names, Deps{Workspace: j, WorkspaceUserID: "local"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := map[string]tool.Tool{}
	for _, b := range built {
		byName[b.Name()] = b
	}
	return byName, j.Root()
}

// assertNoHostPath is the grep the task asks for: the workspace root, the chat id
// and the node id must appear in NO error the model is shown.
func assertNoHostPath(t *testing.T, msg, root string) {
	t.Helper()
	for _, secret := range []string{root, leakChatID, leakNodeID} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks the host namespace (%q):\n  %s", secret, msg)
		}
	}
}

// runTool invokes a built tool the way the model does — through its own Run,
// under a real gated node (which is what puts the chat id and the node dir into
// the resolved path in the first place).
func runTool(t *testing.T, tools map[string]tool.Tool, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	rt, ok := tools[name].(runnableTool)
	if !ok {
		t.Fatalf("tool %q is not runnable", name)
	}
	ctx := confirmlessCtx{newGatedCtx(t, "plan-1", leakNodeID, leakChatID)}
	return rt.Run(ctx, args)
}

// A DIRECT read_file on a missing path: the error names the path the MODEL used,
// and nothing of the host.
func TestReadFileErrorNamesTheModelPathNotTheHostPath(t *testing.T) {
	tools, root := buildToolsForLeakTest(t, "read_file")

	_, err := runTool(t, tools, "read_file", map[string]any{"path": "internal/tools/registry.go"})
	if err == nil {
		t.Fatal("read_file on a missing path returned no error")
	}
	msg := err.Error()
	assertNoHostPath(t, msg, root)
	if !strings.Contains(msg, "internal/tools/registry.go") {
		t.Errorf("error does not name the path the model asked for:\n  %s", msg)
	}
	// Still actionable: the model must be able to tell WHAT went wrong.
	if !strings.Contains(msg, "no such file") {
		t.Errorf("error is no longer actionable — it must still say what went wrong:\n  %s", msg)
	}
}

// THE STRUCTURAL GUARANTEE. The leak is not any tool's bug — it comes from os/git
// handing back the resolved path, which every tool faithfully wraps with %w. So the
// scrub is applied at Build's ONE wrap point, and every tool Build produces carries
// it. A tool added tomorrow is covered without its author knowing the wrapper
// exists; a tool that somehow skips the wrap point fails here.
func TestEveryBuiltToolIsPathScrubbed(t *testing.T) {
	var names []string
	for name, ctor := range registry {
		// Only the tools a workspace-only Deps can actually build (the fs/git/exec
		// ones — the rest need a backend, a model, an advisor).
		if _, err := ctor(Deps{Workspace: mustJail(t), WorkspaceUserID: "local"}); err == nil {
			names = append(names, name)
		}
	}
	built, _ := buildToolsForLeakTest(t, names...)
	if len(built) < len(names) {
		t.Fatalf("Build produced %d tools for %d names", len(built), len(names))
	}
	for name, tl := range built {
		if !scrubbed(tl) {
			t.Errorf("tool %q is not host-path scrubbed — an error from it can leak the workspace root, "+
				"the chat id and the node id into the model's context (see hostpath.go)", name)
		}
	}
}

// scrubbed walks a built tool's wrapper chain (cancel guard → guard ladder →
// scrub → the tool) looking for the scrub.
func scrubbed(t tool.Tool) bool {
	for {
		switch v := t.(type) {
		case *pathScrub:
			return true
		case *cancelGuard:
			t = v.inner
		case *repeatGuard:
			t = v.inner
		case *guardedTool:
			t = v.inner
		default:
			return false
		}
	}
}

func mustJail(t *testing.T) *workspace.Jail {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// confirmlessCtx serves a nil ToolConfirmation (no pending confirm), which the
// functiontool runner consults on every call — the mock alone panics on it.
type confirmlessCtx struct{ *gatedCtx }

func (confirmlessCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
