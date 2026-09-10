package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSandboxRun_ModeNone is the integration test the task calls for: a
// real `quack sandbox run --mode none "echo ok"` against a minimal on-disk
// quack.yaml, asserting exit 0 and the command's output - must pass on a
// dev box with no bwrap/landlock/container available.
func TestSandboxRun_ModeNone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "quack.yaml")
	cfg := `
providers:
  default:
    kind: openai
    endpoint: http://localhost:1
    api_key: x
orchestrator:
  provider: default
  model: m
models:
  m:
    provider: default
    role: worker
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: m
    acp:
      command: ["opencode", "acp"]
      read_only: true
stores:
  default:
    kind: sqlite
    url: ` + filepath.Join(dir, "store.db") + `
session:
  store: default
workspace:
  root: ` + filepath.Join(dir, "workspace") + `
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)

	var out bytes.Buffer
	c := newSandboxCmd()
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"run", "--mode", "none", "echo ok"})

	if err := c.Execute(); err != nil {
		t.Fatalf("sandbox run --mode none: %v\noutput:\n%s", err, out.String())
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("ok")) {
		t.Errorf("expected output to contain %q, got:\n%s", "ok", got)
	}
}

// Flag-registration only: running the real probes risks a FAIL calling
// exitIfNonZero, which would kill the test binary too.
func TestSandboxCheck_JSONFlagRegistered(t *testing.T) {
	c := newSandboxCheckCmd()
	if c.Flags().Lookup("json") == nil {
		t.Error("sandbox check is missing --json")
	}
}

func TestSandboxInfo_JSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "quack.yaml")
	cfg := `
providers:
  default:
    kind: openai
    endpoint: http://localhost:1
    api_key: x
orchestrator:
  provider: default
  model: m
models:
  m:
    provider: default
    role: worker
agents:
  code-reviewer:
    bundle: agents/code-reviewer
    provider: default
    model: m
    acp:
      command: ["opencode", "acp"]
      read_only: true
stores:
  default:
    kind: sqlite
    url: ` + filepath.Join(dir, "store.db") + `
session:
  store: default
workspace:
  root: ` + filepath.Join(dir, "workspace") + `
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)

	var out bytes.Buffer
	c := newSandboxInfoCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"--mode", "none", "--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("sandbox info --json: %v\noutput:\n%s", err, out.String())
	}

	var got sandboxInfo
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if got.Agent != "code-reviewer" {
		t.Errorf("agent = %q, want code-reviewer", got.Agent)
	}
	if got.Mode != "none" {
		t.Errorf("mode = %q, want none", got.Mode)
	}
	// No ro grants are configured - the nested field must still be `[]`, not
	// `null` (json.Unmarshal only leaves a slice field nil for JSON null).
	if got.ROGrants == nil {
		t.Error("ro_grants decoded as nil - the raw JSON must have been null, not []")
	}
	if len(got.Env) == 0 {
		t.Error("env should not be empty")
	}
}
