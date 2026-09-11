package serve

import "testing"

// TestAcpChildEnvAgentOverridesWorkspace: workspace.env is the deployment-wide
// default; a matching key in the agent's own acp.env is more specific and
// wins - the precedence rule documented on WorkspaceConfig.Env.
func TestAcpChildEnvAgentOverridesWorkspace(t *testing.T) {
	workspaceEnv := map[string]string{"JAVA_HOME": "/opt/jdk-21", "ANDROID_HOME": "/opt/android-sdk"}
	agentEnv := map[string]string{"JAVA_HOME": "/opt/jdk-17-for-this-agent"}

	got := acpChildEnv(workspaceEnv, agentEnv)
	want := []string{"ANDROID_HOME=/opt/android-sdk", "JAVA_HOME=/opt/jdk-17-for-this-agent"}
	if len(got) != len(want) {
		t.Fatalf("acpChildEnv = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("acpChildEnv[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestAcpChildEnvWorkspaceOnly: an agent with no acp.env still gets the
// deployment-wide workspace.env entries.
func TestAcpChildEnvWorkspaceOnly(t *testing.T) {
	got := acpChildEnv(map[string]string{"GOROOT": "/opt/go1.25"}, nil)
	if len(got) != 1 || got[0] != "GOROOT=/opt/go1.25" {
		t.Fatalf("acpChildEnv = %v, want [GOROOT=/opt/go1.25]", got)
	}
}
