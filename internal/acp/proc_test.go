package acp

import "testing"

// TestAgentPath pins that workspace.exec_path reaches the ACP subprocess:
// without it the coding agent cannot RUN the toolchain it writes against
// (only the gate's checks could), so every build error costs a gate round.
func TestAgentPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	if got := agentPath(nil); got != "/usr/bin:/bin" {
		t.Errorf("agentPath(nil) = %q, want the ambient PATH", got)
	}
	// Extras FIRST, matching workspace.childPath - a configured JDK must beat
	// a stale system one.
	got := agentPath([]string{"/opt/jdk-21/bin", "/opt/android-sdk/cmdline-tools/latest/bin"})
	want := "/opt/jdk-21/bin:/opt/android-sdk/cmdline-tools/latest/bin:/usr/bin:/bin"
	if got != want {
		t.Errorf("agentPath = %q, want %q", got, want)
	}
}
