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
	argv := a.wrappedArgv(cwd)
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
		argv := a.wrappedArgv(t.TempDir())
		if len(argv) != 2 || argv[0] != "opencode" || argv[1] != "acp" {
			t.Errorf("mode %q: wrappedArgv = %v, want unchanged [opencode acp]", mode, argv)
		}
	}
}
