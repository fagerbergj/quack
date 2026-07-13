package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// requireSandbox skips LOUDLY when bubblewrap isn't usable on this host — a
// sandbox test that silently no-ops is worse than no test.
func requireSandbox(t *testing.T) {
	t.Helper()
	if _, err := workspace.ResolveSandbox(workspace.SandboxBwrap); err != nil {
		t.Skipf("SKIPPING sandbox test: bubblewrap is not usable here (%v). Install it (Debian: apt-get install bubblewrap) to exercise the OS boundary.", err)
	}
}

// sandboxBinding is an fsBinding whose children run under the given OS boundary.
func sandboxBinding(t *testing.T, j *workspace.Jail, mode workspace.SandboxMode) fsBinding {
	t.Helper()
	caps := workspace.DefaultCaps()
	caps.Sandbox = mode
	caps.HomeDir = t.TempDir()
	return fsBinding{userID: "u1", jail: j, caps: caps}
}

// The sandbox makes the child's CWD one of only two writable paths — and that cwd
// is the product of three things the model never sees as one string: the chat
// scope, the node's INVISIBLE ROOT (workspace.NodeDir), and the session cwd a `cd`
// stored NODE-relative, applied exactly once at the jail join (see cwd.go's
// jailPath). Each half was built by a different change; only their product is the
// actual security property, so pin the whole composition end to end through
// run_command:
//
//   - a worker that `cd`s into its cloned repo must still be able to WRITE there
//     (bind the wrong real dir and every build or test an agent runs breaks), and
//   - a child running in that cwd must NOT be able to write into a SIBLING node's
//     directory even when it names that directory's real host path outright — that
//     path is not merely un-suggested, it does not exist in the child's mount
//     namespace.
//
// The `sandbox: none` control at the end is what gives the second assertion teeth:
// the SAME command, the SAME resolved paths, and the sibling IS clobbered.
//
// (Reaching a sibling node's tree through the deliberate "/"-prefixed escape hatch
// — dir: "/other-node/repo" — remains possible by design: that makes the sibling
// the child's own cwd. The jail decides what is addressable; the sandbox only
// stops a child reaching what was never addressed.)
func TestSandboxBindsTheCdResolvedRepoAndNotASiblingNode(t *testing.T) {
	requireSandbox(t)

	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := sandboxBinding(t, j, workspace.SandboxBwrap)

	// Two CONCURRENT nodes of one plan, in one chat: node-a works in its clone,
	// node-b has a file of its own that node-a must never be able to touch.
	ctxA := newGatedCtx(t, "plan-1", "explorer-a", "chat-1")
	ctxB := newGatedCtx(t, "plan-1", "explorer-b", "chat-1")

	bB := b.withCwd(ctxB)
	if _, err := bB.writeFile(writeFileArgs{Path: "secret.txt", Content: "node-b's own file"}); err != nil {
		t.Fatalf("write_file in node-b: %v", err)
	}
	siblingSecret, err := bB.resolve("secret.txt")
	if err != nil {
		t.Fatalf("resolving node-b's file: %v", err)
	}

	// node-a: a cloned repo under its own (invisible) root, then `cd` into it —
	// the live sequence (git_clone → cd → run_command).
	bA := b.withCwd(ctxA)
	if _, err := bA.writeFile(writeFileArgs{Path: "repo/README.md", Content: "# the repo"}); err != nil {
		t.Fatalf("write_file in node-a: %v", err)
	}
	if _, err := bA.cd(ctxA, cdArgs{Dir: "repo"}); err != nil {
		t.Fatalf("cd repo: %v", err)
	}

	// 1. The sandbox bound the RIGHT real directory: a child running with the
	//    session cwd in force writes into the cloned repo.
	res, err := b.withCwd(ctxA).runCommand(runCommandArgs{Command: "touch built.txt"})
	if err != nil {
		t.Fatalf("run_command in the cd'd repo: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a child could not write in its own cd'd repo (exit %d): %q — the sandbox bound the wrong dir",
			res.ExitCode, res.Output)
	}
	repoDir, err := b.withCwd(ctxA).resolve(".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(repoDir, filepath.Join("chat-1", "explorer-a", "repo")) {
		t.Fatalf("the child's cwd resolved to %q — want the node's own repo (<chat>/<node>/repo)", repoDir)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "built.txt")); err != nil {
		t.Fatalf("the write did not land in the cd'd repo (%s): %v", repoDir, err)
	}

	// 2. …and NOTHING else: naming the sibling node's real host path outright
	//    (no shell metacharacter, so the metachar wall never sees it — the OS
	//    boundary is all that stands here) must fail.
	res, err = b.withCwd(ctxA).runCommand(runCommandArgs{Command: "cp README.md " + siblingSecret})
	if err != nil {
		t.Fatalf("run_command errored (want a clean non-zero exit): %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("SANDBOX ESCAPE: node-a wrote into node-b's directory (%s): %q", siblingSecret, res.Output)
	}
	if got, err := os.ReadFile(siblingSecret); err != nil || string(got) != "node-b's own file" {
		t.Fatalf("node-b's file was modified by node-a: %q (err=%v)", got, err)
	}

	// Control: with the sandbox off, the SAME command clobbers the sibling. If
	// this ever stops passing, the assertion above proves nothing.
	plain := sandboxBinding(t, j, workspace.SandboxNone)
	res, err = plain.withCwd(ctxA).runCommand(runCommandArgs{Command: "cp README.md " + siblingSecret})
	if err != nil {
		t.Fatalf("unsandboxed control run errored: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("the control run should have succeeded (the test would otherwise prove nothing): exit=%d output=%q",
			res.ExitCode, res.Output)
	}
	if got, err := os.ReadFile(siblingSecret); err != nil || !strings.Contains(string(got), "the repo") {
		t.Fatalf("control run should have clobbered the sibling: %q (err=%v)", got, err)
	}
}
