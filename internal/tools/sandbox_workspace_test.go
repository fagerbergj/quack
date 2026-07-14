package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// A sandboxed child must be able to write anywhere in ITS OWN node workspace, and those
// writes must survive the command.
//
// THE LIVE FAILURE (code-mode dogfood, 2026-07-13). The sandbox gives each child a
// private /tmp (workspace.tmpArgs) and bound only the child's exact CWD. Our workspace
// root lives UNDER /tmp:
//
//	QUACK_WORKSPACE_ROOT=/tmp/claude-1000/…/scratchpad/workspace
//
// so inside the sandbox the entire workspace was replaced by the throwaway mount, with
// just the one cwd path bound back on top of it. Everything the child wrote elsewhere in
// its own workspace — a `git clone` into the node dir, a file one directory up — landed
// in the private mount and EVAPORATED when the command exited.
//
// The Go fs tools are not sandboxed and saw the real tree, so the model was handed two
// contradictory views of its own workspace, and blamed the tools:
//
//	"The software-agent-sdk clone is missing from this workspace… I see the workspace
//	 has changed between turns… The cd tool seems to have lost its state or the path
//	 resolution is broken."
//
// It hadn't. We ate its files.
func TestSandboxedShellWritesSurviveInTheNodesOwnWorkspace(t *testing.T) {
	requireSandbox(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := sandboxBinding(t, jail, workspace.SandboxBwrap)
	ctx := newGatedCtx(t, "plan-1", "explorer-openhands", "chat-1")

	// The node has a repo, and cd's into it — exactly the live sequence.
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "OpenHands/README.md", Content: "# OpenHands"}); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "OpenHands"}); err != nil {
		t.Fatalf("cd: %v", err)
	}

	// From inside the repo, the model writes ONE DIRECTORY UP — into its own node dir.
	// This is what `git clone ../software-agent-sdk` does, and it is what vanished.
	res, err := b.withCwd(ctx).runCommand(runCommandArgs{
		Command: "mkdir -p ../software-agent-sdk && echo cloned > ../software-agent-sdk/marker.txt",
	})
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("write one dir up failed (exit %d): %s", res.ExitCode, res.Output)
	}

	// It must be THERE, on the real filesystem, after the child exited — visible to
	// the (unsandboxed) fs tools, which is where the model looks next.
	got, err := b.withCwd(ctx).readFile(readFileArgs{Path: "/software-agent-sdk/marker.txt"})
	if err != nil {
		t.Fatalf("the sandboxed shell's write did not survive: %v\n"+
			"the child wrote into a private mount and the file evaporated — this is the live bug", err)
	}
	if got.Content != "cloned\n" {
		t.Fatalf("marker.txt = %q, want %q", got.Content, "cloned\n")
	}
}

// ...but the node's own workspace is the LIMIT. A sibling node's tree stays unreachable:
// widening the bind from the cwd to the node dir must not widen it to the chat.
func TestSandboxedShellStillCannotReachASiblingNode(t *testing.T) {
	requireSandbox(t)
	root := t.TempDir()
	jail, err := workspace.NewJail(root)
	if err != nil {
		t.Fatal(err)
	}
	b := sandboxBinding(t, jail, workspace.SandboxBwrap)

	// Sibling node writes a secret in ITS own dir.
	sibCtx := newGatedCtx(t, "plan-1", "explorer-goose", "chat-1")
	if _, err := b.withCwd(sibCtx).writeFile(writeFileArgs{Path: "secret.txt", Content: "sibling"}); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	sibPath, err := b.withCwd(sibCtx).resolve("secret.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Our node's sandboxed shell names the sibling's REAL absolute path. The OS
	// boundary — not a string filter — must stop it.
	ours := newGatedCtx(t, "plan-1", "explorer-openhands", "chat-1")
	if _, err := b.withCwd(ours).writeFile(writeFileArgs{Path: "keep.txt", Content: "x"}); err != nil {
		t.Fatalf("seed ours: %v", err)
	}
	res, err := b.withCwd(ours).runCommand(runCommandArgs{
		Command: "echo pwned > " + sibPath,
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatal("a sandboxed shell wrote into a SIBLING node's directory — the node dir must be the writable limit")
	}
	data, rerr := os.ReadFile(filepath.Clean(sibPath))
	if rerr != nil {
		t.Fatalf("read sibling: %v", rerr)
	}
	if string(data) != "sibling" {
		t.Fatalf("the sibling's file was modified: %q", data)
	}
}
