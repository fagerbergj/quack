package acp

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestSpawnEnvIsHermetic: PATH is workspace.ChildPath(caps) in EVERY sandbox
// mode, never the server's ambient PATH - the toolchain the agent needs to
// run is already covered by Caps.ExtraPath + the fixed system dirs, so
// ambient added no reach a leak couldn't also use.
func TestSpawnEnvIsHermetic(t *testing.T) {
	t.Setenv("PATH", "/totally/not/hermetic")
	for _, mode := range []workspace.SandboxMode{"", workspace.SandboxNone, workspace.SandboxBwrap, workspace.SandboxLandlock} {
		caps := workspace.Caps{Sandbox: mode, ExtraPath: []string{"/opt/jdk-21/bin"}}
		a := &Agent{opts: Options{Caps: caps, Home: "/home/agent"}}
		env := a.spawnEnv(a.opts.Caps)
		want := "PATH=" + workspace.ChildPath(caps)
		found := false
		for _, e := range env {
			if e == want {
				found = true
			}
			if strings.Contains(e, "/totally/not/hermetic") {
				t.Errorf("mode %q: ambient PATH leaked into spawnEnv: %q", mode, e)
			}
		}
		if !found {
			t.Errorf("mode %q: spawnEnv() = %v, want %q present", mode, env, want)
		}
	}
}

// TestSpawnEnvTracksRoundScratchDir pins the writable-scratch fix: spawnEnv
// must build TMPDIR from THIS round's caps (the parameter, with per-node
// ScratchDir already resolved by resolveNode/runPrompt), not the agent's
// static a.opts.Caps - before this fix TMPDIR always read a.opts.Caps, so a
// round's ScratchDir override never reached the subprocess's actual
// environment despite being correctly computed and granted.
func TestSpawnEnvTracksRoundScratchDir(t *testing.T) {
	home := t.TempDir()
	static := workspace.Caps{Sandbox: workspace.SandboxLandlock, HomeDir: home}
	a := &Agent{opts: Options{Caps: static, Home: home}}

	roundCaps := static
	roundCaps.ScratchDir = t.TempDir()

	var got string
	for _, e := range a.spawnEnv(roundCaps) {
		if strings.HasPrefix(e, "TMPDIR=") {
			got = e
		}
	}
	want := "TMPDIR=" + roundCaps.ScratchDir
	if got != want {
		t.Errorf("spawnEnv(roundCaps) TMPDIR = %q, want %q (the round's scratch dir, not the agent's static caps)", got, want)
	}
}

// TestWrappedArgvLandlock: mode landlock wraps Command through the Landlock
// shim - argv[1] is the SandboxExecArg dispatch, and the node dir (cwd) is
// granted, with the original command preserved past "--".
func TestWrappedArgvLandlock(t *testing.T) {
	cwd := t.TempDir()
	a := &Agent{opts: Options{
		Command: []string{"opencode", "acp"},
		Caps:    workspace.Caps{Sandbox: workspace.SandboxLandlock},
		ExtraRO: []string{"/skills"},
	}}
	argv := a.wrappedArgv(cwd, nil, a.opts.Caps)
	if len(argv) < 2 || argv[1] != workspace.SandboxExecArg {
		t.Fatalf("wrappedArgv landlock = %v, want argv[1] == %q", argv, workspace.SandboxExecArg)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, cwd) {
		t.Errorf("wrappedArgv landlock = %v, want the node dir %q granted", argv, cwd)
	}
	if !strings.HasSuffix(joined, "opencode acp") {
		t.Errorf("wrappedArgv landlock = %v, want the original command preserved past --", argv)
	}
}

// TestWrappedArgvBwrap (#921): bwrap wraps the ACP child too, as identity bind
// mounts - the node dir bound at its own path (never SandboxWorkRoot: quack and
// opencode trade absolute paths over JSON-RPC), the skill paths read-only, and
// the original command past "--".
func TestWrappedArgvBwrap(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	a := &Agent{opts: Options{
		Command: []string{"opencode", "acp"},
		Caps:    workspace.Caps{Sandbox: workspace.SandboxBwrap, HomeDir: home},
		ExtraRO: []string{"/skills"},
	}}
	argv := a.wrappedArgv(cwd, nil, a.opts.Caps)
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"--bind-try\x00" + cwd + "\x00" + cwd, "--bind-try\x00" + home, "--ro-bind-try\x00/skills"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrappedArgv bwrap = %v, missing %q", argv, strings.ReplaceAll(want, "\x00", " "))
		}
	}
	if strings.Contains(joined, workspace.SandboxWorkRoot) {
		t.Errorf("wrappedArgv bwrap remapped the node dir onto %s: %v", workspace.SandboxWorkRoot, argv)
	}
	if !strings.HasSuffix(strings.Join(argv, " "), "-- opencode acp") {
		t.Errorf("wrappedArgv bwrap = %v, want the original command preserved past --", argv)
	}
}

// TestWrappedArgvUnwrappedUnderNone: `none` has no boundary to wrap into, so
// Command is spawned exactly as configured.
func TestWrappedArgvUnwrappedUnderNone(t *testing.T) {
	for _, mode := range []workspace.SandboxMode{workspace.SandboxNone, ""} {
		a := &Agent{opts: Options{Command: []string{"opencode", "acp"}, Caps: workspace.Caps{Sandbox: mode}}}
		argv := a.wrappedArgv(t.TempDir(), nil, a.opts.Caps)
		if len(argv) != 2 || argv[0] != "opencode" || argv[1] != "acp" {
			t.Errorf("mode %q: wrappedArgv = %v, want unchanged [opencode acp]", mode, argv)
		}
	}
}

// TestSpawnEnvPinsJavaTmpDir: the JVM ignores TMPDIR (java.io.tmpdir is /tmp on
// Linux), and landlock never grants the real /tmp - so without JAVA_TOOL_OPTIONS
// every JVM build the agent runs dies in a static initialiser writing there
// (Room's KSP: "AccessDeniedException: /tmp/...libsqlitejdbc.so.lck" surfacing as
// ExceptionInInitializerError). Only landlock needs it: bwrap remaps /tmp and
// none leaves the real one reachable.
func TestSpawnEnvPinsJavaTmpDir(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		mode workspace.SandboxMode
		want bool
	}{{workspace.SandboxLandlock, true}, {workspace.SandboxBwrap, false}, {workspace.SandboxNone, false}} {
		caps := workspace.Caps{Sandbox: tc.mode, HomeDir: home}
		a := &Agent{opts: Options{Caps: caps, Home: home}}
		var got string
		for _, e := range a.spawnEnv(a.opts.Caps) {
			if strings.HasPrefix(e, "JAVA_TOOL_OPTIONS=") {
				got = e
			}
		}
		if tc.want {
			if want := "JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=" + workspace.SandboxTmpDir(caps); got != want {
				t.Errorf("mode %q: got %q, want %q", tc.mode, got, want)
			}
		} else if got != "" {
			t.Errorf("mode %q: JAVA_TOOL_OPTIONS should not be set, got %q", tc.mode, got)
		}
	}
}

// TestSpawnEnvOperatorOverridesJavaToolOptions: opts.Env (workspace.env) comes
// last so an operator who sets JAVA_TOOL_OPTIONS themselves still wins.
func TestSpawnEnvOperatorOverridesJavaToolOptions(t *testing.T) {
	home := t.TempDir()
	a := &Agent{opts: Options{
		Caps: workspace.Caps{Sandbox: workspace.SandboxLandlock, HomeDir: home},
		Home: home,
		Env:  []string{"JAVA_TOOL_OPTIONS=-Xmx512m"},
	}}
	env := a.spawnEnv(a.opts.Caps)
	var last string
	for _, e := range env {
		if strings.HasPrefix(e, "JAVA_TOOL_OPTIONS=") {
			last = e
		}
	}
	if last != "JAVA_TOOL_OPTIONS=-Xmx512m" {
		t.Errorf("operator JAVA_TOOL_OPTIONS did not win: got %q", last)
	}
}

// TestWrappedArgvBwrapAcpHandshake (#921) is the one thing an argv assertion
// cannot prove: that wrapping the ACP child in a bwrap namespace does not
// break the protocol it speaks. It spawns a REAL `opencode acp` through the
// production seam pair (wrappedArgv + spawnEnv), writes an ACP `initialize`
// frame to its stdin and reads the reply off stdout - no LLM endpoint needed,
// the handshake precedes any model call. Skips (loudly) where bwrap or
// opencode is unavailable, like every other sandbox test here.
func TestWrappedArgvBwrapAcpHandshake(t *testing.T) {
	if _, err := workspace.ResolveSandbox(workspace.SandboxBwrap); err != nil {
		t.Skipf("SKIPPING ACP sandbox test: bubblewrap is not usable here (%v)", err)
	}
	opencode, err := exec.LookPath("opencode")
	if err != nil {
		t.Skipf("SKIPPING ACP sandbox test: opencode is not on PATH (%v)", err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("SKIPPING ACP sandbox test: node is not on PATH (%v)", err)
	}

	cwd, home := t.TempDir(), t.TempDir()
	caps := workspace.Caps{
		Sandbox: workspace.SandboxBwrap, WorkRoot: cwd, HomeDir: home,
		ScratchDir: filepath.Join(home, "tmp", "handshake"), ReadOnly: true,
		// In the image opencode and node live under /usr (already in the
		// sandbox's system view); a dev host keeps them elsewhere, which is
		// exactly what exec_path/ExtraPath is for.
		ExtraPath: []string{filepath.Dir(opencode), filepath.Dir(node)},
	}
	a := &Agent{opts: Options{Command: []string{opencode, "acp"}, Caps: caps, Home: home}}

	argv := a.wrappedArgv(cwd, nil, caps)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = a.spawnEnv(caps)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr tailBuffer
	stderr.max = 4096
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn wrapped opencode: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	go func() {
		_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":0,"method":"initialize",` +
			`"params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false}}}}` + "\n"))
	}()
	reply := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		reply <- line
	}()
	select {
	case line := <-reply:
		if !strings.Contains(line, `"result"`) || !strings.Contains(line, "protocolVersion") {
			t.Fatalf("ACP initialize did not succeed through the bwrap wrap: %q\nstderr: %s", line, stderr.String())
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("no ACP initialize reply through the bwrap wrap in 90s\nstderr: %s", stderr.String())
	}
}
