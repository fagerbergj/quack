package tools

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// ONE namespace — including inside the shell.
//
// THE LIVE FAILURE (code-implementer, 2026-07-13). PR #213 gave run_command a real
// shell inside the sandbox, and that shell's child saw the REAL HOST PATHS: `pwd`
// printed
//
//	/tmp/claude-1000/-home-jason-workspace-agent-researcher/…/workspace/local/<chatID>/<nodeID>/quack
//
// while every fs tool called that exact directory `/quack`. Two names for one place
// is precisely what #204/#209 spent two PRs removing, and the model did what it did
// before — it flailed, hunting the host filesystem for its own workspace:
//
//	run_command  pwd && find /tmp -maxdepth 4 -name "quack" -type d
//	run_command  find /home/jason/workspace/agent-researcher/… …
//	run_command  find /tmp -name "quack" -type d 2>/dev/null | head
//	git_clone    quack        ← gave up and cloned the repo A SECOND TIME
//
// So the shell must speak the model's namespace: the node's own directory (the
// invisible root every fs tool resolves against) is mounted at ONE fixed path inside
// the sandbox — workspace.SandboxWorkRoot — and the child is chdir'd relative to it.
func TestSandboxedShellPrintsTheModelsPathNotTheHosts(t *testing.T) {
	requireSandbox(t)
	root := t.TempDir()
	jail, err := workspace.NewJail(root)
	if err != nil {
		t.Fatal(err)
	}
	b := sandboxBinding(t, jail, workspace.SandboxBwrap)
	ctx := newGatedCtx(t, "plan-1", "coder-quack", "chat-1")

	// The live sequence: a repo in the node's workspace, then `cd` into it.
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "quack/README.md", Content: "# quack\n"}); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	cdRes, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "quack"})
	if err != nil {
		t.Fatalf("cd: %v", err)
	}
	if cdRes.Cwd != "/quack" {
		t.Fatalf("cd reported cwd %q, want %q (the model's namespace)", cdRes.Cwd, "/quack")
	}

	res, err := b.withCwd(ctx).runCommand(runCommandArgs{Command: "pwd"})
	if err != nil {
		t.Fatalf("run_command(pwd): %v", err)
	}
	pwd := strings.TrimSpace(res.Output)

	// It must END where the tools say it is…
	if !strings.HasSuffix(pwd, cdRes.Cwd) {
		t.Errorf("pwd = %q, want it to end in the tools' own cwd %q — the shell and the tools must name one place once",
			pwd, cdRes.Cwd)
	}
	// …and carry NOTHING of the host: no workspace root, no chat id, no node id.
	// Each of those is a string the model has been observed to grep the host
	// filesystem for.
	for _, leak := range []string{root, "chat-1", "coder-quack"} {
		if strings.Contains(pwd, leak) {
			t.Errorf("pwd = %q leaks %q into the model's context — the child must not see the host layout", pwd, leak)
		}
	}
	if pwd != workspace.SandboxWorkRoot+"/quack" {
		t.Errorf("pwd = %q, want the fixed mount %q (stable across runs, chats and nodes)",
			pwd, workspace.SandboxWorkRoot+"/quack")
	}
}

// The load-bearing rule of the one namespace: a path read out of ANY tool result can
// be fed straight back into ANY tool — and the shell is one of those tools, in both
// directions.
func TestPathsRoundTripBetweenTheShellAndTheTools(t *testing.T) {
	requireSandbox(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := sandboxBinding(t, jail, workspace.SandboxBwrap)
	ctx := newGatedCtx(t, "plan-1", "coder-quack", "chat-1")

	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "quack/README.md", Content: "# quack\n"}); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	cdRes, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "quack"})
	if err != nil {
		t.Fatalf("cd: %v", err)
	}

	// 1. TOOL → SHELL: the cwd `cd` reported ("/quack"), used VERBATIM in a shell
	//    command, names the same directory.
	res, err := b.withCwd(ctx).runCommand(runCommandArgs{Command: "cat " + cdRes.Cwd + "/README.md"})
	if err != nil {
		t.Fatalf("run_command(cat %s/README.md): %v", cdRes.Cwd, err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "# quack") {
		t.Fatalf("a path from a TOOL result did not work in the SHELL: exit=%d output=%q", res.ExitCode, res.Output)
	}

	// 2. SHELL → TOOL: a path the shell printed, fed straight back to read_file.
	res, err = b.withCwd(ctx).runCommand(runCommandArgs{Command: "pwd"})
	if err != nil {
		t.Fatalf("run_command(pwd): %v", err)
	}
	shellPath := strings.TrimSpace(res.Output) + "/README.md"
	got, err := b.withCwd(ctx).readFile(readFileArgs{Path: shellPath})
	if err != nil {
		t.Fatalf("a path the SHELL printed (%s) did not work in read_file: %v", shellPath, err)
	}
	if got.Content != "# quack\n" {
		t.Fatalf("read_file(%s) = %q, want the repo's README", shellPath, got.Content)
	}

	// 3. …and both spellings name the SAME file: the sandbox mount is an alias of
	//    the model's root, not a second namespace.
	if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "/quack/README.md"}); err != nil {
		t.Fatalf("the model's own spelling stopped working: %v", err)
	}
}

// jailPath is the ONE place a model-written path is resolved, so it must accept the
// sandbox spelling of the root as what it is: an alias for "/". Unit-level, so it
// holds on hosts without bubblewrap too.
func TestJailPathAcceptsTheSandboxMountSpelling(t *testing.T) {
	node := "chat/node-1"
	for _, c := range []struct{ in, want string }{
		{"/quack/main.go", "chat/node-1/quack/main.go"},
		{workspace.SandboxWorkRoot + "/quack/main.go", "chat/node-1/quack/main.go"},
		{workspace.SandboxWorkRoot, "chat/node-1"},
		{workspace.SandboxWorkRoot + "/", "chat/node-1"},
		// A relative path is never touched: a directory literally named
		// "workspace" is still addressable, just not through the absolute alias.
		{"workspace/x", "chat/node-1/workspace/x"},
	} {
		if got := jailPath(node, "", c.in); got != c.want {
			t.Errorf("jailPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
