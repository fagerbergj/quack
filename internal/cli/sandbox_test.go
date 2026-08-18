package cli

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

func TestNormalizeSandboxMode(t *testing.T) {
	cases := []struct {
		in      string
		want    workspace.SandboxMode
		wantErr bool
	}{
		{"", "", false},
		{"none", workspace.SandboxNone, false},
		{"bwrap", workspace.SandboxBwrap, false},
		{"landlock", workspace.SandboxLandlock, false},
		{"nonsense", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeSandboxMode(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("NormalizeSandboxMode(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("NormalizeSandboxMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func testAgentConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.AgentConfig{
			"code-reviewer": {Acp: &config.AcpAgentConfig{ReadOnly: true}},
			"code-writer":   {Acp: &config.AcpAgentConfig{ReadOnly: false}},
		},
	}
}

func TestResolveSandboxAgent(t *testing.T) {
	cfg := testAgentConfig()

	name, ac, err := ResolveSandboxAgent(cfg, "")
	if err != nil {
		t.Fatalf("default agent: %v", err)
	}
	if name != DefaultSandboxAgent || !ac.Acp.ReadOnly {
		t.Errorf("default agent = %q readonly=%v, want %q readonly=true", name, ac.Acp.ReadOnly, DefaultSandboxAgent)
	}

	name, ac, err = ResolveSandboxAgent(cfg, "code-writer")
	if err != nil || name != "code-writer" || ac.Acp.ReadOnly {
		t.Errorf("named agent: name=%q ac=%+v err=%v", name, ac, err)
	}

	if _, _, err := ResolveSandboxAgent(cfg, "no-such-agent"); err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestSandboxPS1(t *testing.T) {
	if got := SandboxPS1("code-reviewer", true); got != "[quack:code-reviewer ro] $ " {
		t.Errorf("ro PS1 = %q", got)
	}
	if got := SandboxPS1("code-writer", false); got != "[quack:code-writer rw] $ " {
		t.Errorf("rw PS1 = %q", got)
	}
}

func TestResolveSandboxSeat_ModeNone(t *testing.T) {
	root := t.TempDir()
	jail, err := workspace.NewJail(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testAgentConfig()
	cfg.Workspace.Sandbox = "none"

	seat, err := ResolveSandboxSeat(cfg, jail, "", "", "")
	if err != nil {
		t.Fatalf("ResolveSandboxSeat: %v", err)
	}
	if seat.AgentName != DefaultSandboxAgent {
		t.Errorf("agent = %q", seat.AgentName)
	}
	if !seat.ReadOnly {
		t.Error("code-reviewer should resolve read-only")
	}
	if !seat.FreshDir || seat.Dir == "" {
		t.Errorf("expected a freshly minted dir, got %+v", seat)
	}
	if seat.Caps.Sandbox != workspace.SandboxNone {
		t.Errorf("caps.Sandbox = %q", seat.Caps.Sandbox)
	}
	seat.Cleanup()
}

func TestResolveSandboxSeat_CwdDot(t *testing.T) {
	root := t.TempDir()
	jail, err := workspace.NewJail(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testAgentConfig()
	cfg.Workspace.Sandbox = "none"

	seat, err := ResolveSandboxSeat(cfg, jail, "code-writer", ".", "")
	if err != nil {
		t.Fatalf("ResolveSandboxSeat: %v", err)
	}
	if seat.FreshDir {
		t.Error("--cwd . must not be treated as freshly minted (must survive Cleanup)")
	}
	seat.Cleanup() // must not remove the real cwd
}

func TestResolveSandboxSeat_BadMode(t *testing.T) {
	root := t.TempDir()
	jail, _ := workspace.NewJail(root)
	cfg := testAgentConfig()
	if _, err := ResolveSandboxSeat(cfg, jail, "", "", "bogus"); err == nil {
		t.Error("expected error for bad --mode")
	}
}

func TestSandboxSpawnEnv(t *testing.T) {
	caps := workspace.Caps{HomeDir: "/home/x", Env: map[string]string{"A": "1"}}
	ac := config.AgentConfig{Acp: &config.AcpAgentConfig{Env: map[string]string{"B": "2", "A": "override"}}}
	env := SandboxSpawnEnv(caps, ac, map[string]string{"PS1": "prompt"})

	want := map[string]bool{"A=override": false, "B=2": false, "PS1=prompt": false, "HOME=/home/x": false}
	for _, kv := range env {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for kv, found := range want {
		if !found {
			t.Errorf("missing %q in env %v", kv, env)
		}
	}
}
