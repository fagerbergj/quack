package acp

import (
	"strings"
	"testing"

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
		env := a.spawnEnv()
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
	argv := a.wrappedArgv(cwd, nil)
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

// TestWrappedArgvUnwrappedOutsideLandlock: none/bwrap pass Command through
// unchanged - bwrap for the ACP path is a documented ponytail ceiling (see
// workspace.WrapArgv's doc), and none has no boundary to wrap into.
func TestWrappedArgvUnwrappedOutsideLandlock(t *testing.T) {
	for _, mode := range []workspace.SandboxMode{workspace.SandboxNone, workspace.SandboxBwrap, ""} {
		a := &Agent{opts: Options{Command: []string{"opencode", "acp"}, Caps: workspace.Caps{Sandbox: mode}}}
		argv := a.wrappedArgv(t.TempDir(), nil)
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
		for _, e := range a.spawnEnv() {
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
	env := a.spawnEnv()
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
