package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestServerValidate_JSON(t *testing.T) {
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

	var out bytes.Buffer
	c := newServerValidateCmd()
	c.SetOut(&out)
	c.SetArgs([]string{cfgPath, "--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("server validate --json: %v\noutput:\n%s", err, out.String())
	}

	var got serverValidateResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if got.Path != cfgPath {
		t.Errorf("path = %q, want %q", got.Path, cfgPath)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
}

// An invalid config must still error (not silently report ok in JSON).
func TestServerValidate_JSONInvalid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "quack.yaml")
	if err := os.WriteFile(cfgPath, []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := newServerValidateCmd()
	c.SilenceUsage = true
	c.SetOut(&out)
	c.SetArgs([]string{cfgPath, "--json"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected an error for an invalid config")
	}
	if out.Len() != 0 {
		t.Errorf("invalid config should print no JSON status, got %q", out.String())
	}
}
