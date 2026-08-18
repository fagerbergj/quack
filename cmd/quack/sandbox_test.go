package main

import (
	"bytes"
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
