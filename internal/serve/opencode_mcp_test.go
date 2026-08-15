package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// opencodeEnv must emit an acp mcp_servers URL in opencode's KEYED mcp map shape
// - {name:{type:"remote",url,enabled}}, the form opencode.json uses - not
// {"servers":[...]}, which opencode silently ignores (the server would never
// load, so the tools never reach the agent). Regression for #250: the first
// implementation compiled and passed the gate but used the ignored shape.
func TestOpencodeEnvMcpServersShape(t *testing.T) {
	env := opencodeEnv(config.ProviderConfig{}, config.AgentConfig{
		Model: "m",
		Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}, McpServers: []string{"https://mcp.context7.com/mcp"}},
	}, nil, workspace.SandboxLandlock)
	if len(env) != 1 || !strings.HasPrefix(env[0], "OPENCODE_CONFIG_CONTENT=") {
		t.Fatalf("unexpected env: %v", env)
	}
	raw := strings.TrimPrefix(env[0], "OPENCODE_CONFIG_CONTENT=")

	var cfg struct {
		Mcp map[string]struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT is not the keyed mcp shape (%v):\n%s", err, raw)
	}
	if _, wrong := cfg.Mcp["servers"]; wrong {
		t.Fatalf("mcp used the ignored {servers:[...]} shape:\n%s", raw)
	}
	c7, ok := cfg.Mcp["context7"]
	if !ok {
		t.Fatalf("context7 not keyed into the mcp map:\n%s", raw)
	}
	if c7.Type != "remote" || c7.URL != "https://mcp.context7.com/mcp" || !c7.Enabled {
		t.Fatalf("context7 entry wrong: %+v", c7)
	}
}

// opencodePermissions decodes the generated config's permission block.
func opencodePermissions(t *testing.T, ac config.AgentConfig, sandbox workspace.SandboxMode) (bash map[string]string, extDir map[string]string) {
	t.Helper()
	env := opencodeEnv(config.ProviderConfig{}, ac, nil, sandbox)
	raw := strings.TrimPrefix(env[0], "OPENCODE_CONFIG_CONTENT=")
	var cfg struct {
		Permission struct {
			Bash              map[string]string `json:"bash"`
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT: %v:\n%s", err, raw)
	}
	return cfg.Permission.Bash, cfg.Permission.ExternalDirectory
}

var sandboxModes = []workspace.SandboxMode{workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone}

// assertClosedShape: the pre-allow_clone permission block - clone and push denied,
// cwd is the external_directory boundary.
func assertClosedShape(t *testing.T, bash, extDir map[string]string) {
	t.Helper()
	for _, denied := range []string{
		"git push", "git push *",
		"git clone", "git clone *",
		"gh repo clone", "gh repo clone *",
	} {
		if got := bash[denied]; got != "deny" {
			t.Errorf("bash permission[%q] = %q, want %q", denied, got, "deny")
		}
	}
	if bash["*"] != "allow" {
		t.Errorf(`bash permission["*"] = %q, want "allow"`, bash["*"])
	}
	if extDir["*"] != "deny" {
		t.Errorf(`external_directory["*"] = %q, want "deny" (cwd is the boundary)`, extDir["*"])
	}
}

// TestOpencodeEnvDeniesCloneAndPush pins the clone-deny half of the worktree-isolation
// follow-up for every agent that can WRITE code: cloning is unnecessary now that the
// environment block (internal/acp's environmentBlock) shows the agent what's already
// on disk, and denied for the same reason git push already is - mirror the git push
// deny's exact shape (bare command + wildcard variant) for git clone and gh repo clone.
// No sandbox mode changes this for an agent without allow_clone.
func TestOpencodeEnvDeniesCloneAndPush(t *testing.T) {
	for _, mode := range sandboxModes {
		t.Run(string(mode), func(t *testing.T) {
			bash, extDir := opencodePermissions(t, config.AgentConfig{
				Model: "m",
				Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}},
			}, mode)
			assertClosedShape(t, bash, extDir)
		})
	}
}

// TestOpencodeEnvAllowsCloneForAllowCloneAgent: the code-explorer is chartered to read
// third-party repos the gate never provisions, so acp.allow_clone lifts the clone deny
// (and the cwd-only external_directory boundary, since the clone lands in $TMPDIR) - in
// every mode that OS-enforces the RO work tree the wide external_directory rests on.
// Since #921 that is landlock AND bwrap, both of which workspace.WrapArgv wraps.
// git push stays denied - allow_clone is about reading, never delivering.
func TestOpencodeEnvAllowsCloneForAllowCloneAgent(t *testing.T) {
	for _, mode := range []workspace.SandboxMode{workspace.SandboxLandlock, workspace.SandboxBwrap} {
		t.Run(string(mode), func(t *testing.T) {
			bash, extDir := opencodePermissions(t, config.AgentConfig{
				Model: "m",
				Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}, ReadOnly: true, AllowClone: true},
			}, mode)
			for _, cmd := range []string{"git clone", "git clone *", "gh repo clone", "gh repo clone *"} {
				if got, ok := bash[cmd]; ok {
					t.Errorf("bash permission[%q] = %q, want no deny entry (falls through to the %q allow)", cmd, got, "*")
				}
			}
			for _, denied := range []string{"git push", "git push *"} {
				if got := bash[denied]; got != "deny" {
					t.Errorf("bash permission[%q] = %q, want %q - delivery stays gate-owned", denied, got, "deny")
				}
			}
			if extDir["*"] != "allow" {
				t.Errorf(`external_directory["*"] = %q, want "allow" (the clone lives outside cwd; the sandbox is the boundary)`, extDir["*"])
			}
		})
	}
}

// TestOpencodeEnvAllowCloneNeedsABoundary: under `none` there is no OS boundary at
// all, so `external_directory: allow` would let opencode's file tools read and write
// anywhere the server user can - ~/.ssh, ~/.aws, .env. allow_clone degrades to the
// pre-#917 closed shape instead rather than to unbounded.
func TestOpencodeEnvAllowCloneNeedsABoundary(t *testing.T) {
	bash, extDir := opencodePermissions(t, config.AgentConfig{
		Model: "m",
		Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}, ReadOnly: true, AllowClone: true},
	}, workspace.SandboxNone)
	assertClosedShape(t, bash, extDir)
}

// TestOpencodeEnvReadOnlyAloneDoesNotAllowClone: allow_clone is the ONLY key that
// lifts the deny. read_only alone (the code-reviewer's shape) keeps clone denied -
// the reviewer works on the gate-provisioned worktree and has no business cloning.
func TestOpencodeEnvReadOnlyAloneDoesNotAllowClone(t *testing.T) {
	for _, mode := range sandboxModes {
		t.Run(string(mode), func(t *testing.T) {
			bash, extDir := opencodePermissions(t, config.AgentConfig{
				Model: "m",
				Acp:   &config.AcpAgentConfig{Command: []string{"opencode", "acp"}, ReadOnly: true},
			}, mode)
			assertClosedShape(t, bash, extDir)
		})
	}
}

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
