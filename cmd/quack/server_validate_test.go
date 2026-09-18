package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// `server validate` resolves plugins.seed and lists each plugin's seeded
// agents and shapes in both --json and plain-text output.
func TestServerValidate_ListsPluginSeededAgentsAndShapes(t *testing.T) {
	dir := t.TempDir()
	// A local plugin root's registry name is its directory's base name, never
	// plugin.json's own "name" field - so this dir must be named "acme".
	pluginDir := mustMkdir(t, filepath.Join(dir, "acme"))
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"$schema":"x","name":"acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := mustMkdir(t, filepath.Join(pluginDir, "agents", "scout"))
	if err := os.WriteFile(filepath.Join(agentDir, "agent-card.json"), []byte(`{"name":"scout"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("model_role: researcher\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workflowsDir := mustMkdir(t, filepath.Join(pluginDir, "workflows"))
	shape := "name: acme-job\ntrigger: \"Run the acme job\"\nagents: [scout]\nshape: \"ONE `scout` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
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
plugins:
  seed:
    - ` + pluginDir + `
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
	if len(got.Plugins) != 1 || got.Plugins[0].Plugin != "acme" {
		t.Fatalf("Plugins = %+v, want one acme entry", got.Plugins)
	}
	if len(got.Plugins[0].Agents) != 1 || got.Plugins[0].Agents[0] != "scout" {
		t.Errorf("Plugins[0].Agents = %v, want [scout]", got.Plugins[0].Agents)
	}
	if len(got.Plugins[0].Shapes) != 1 || got.Plugins[0].Shapes[0] != "acme-job" {
		t.Errorf("Plugins[0].Shapes = %v, want [acme-job]", got.Plugins[0].Shapes)
	}

	var plain bytes.Buffer
	c2 := newServerValidateCmd()
	c2.SetOut(&plain)
	c2.SetArgs([]string{cfgPath})
	if err := c2.Execute(); err != nil {
		t.Fatalf("server validate: %v\noutput:\n%s", err, plain.String())
	}
	if !strings.Contains(plain.String(), "acme") || !strings.Contains(plain.String(), "scout") {
		t.Errorf("plain output %q must list the plugin's seeded agent", plain.String())
	}
}

// A plugins.seed row declaring an unlinked module fails ResolveConfiguredPlugins,
// and `server validate` must surface that error rather than reporting ok.
func TestServerValidate_UnlinkedPluginModuleErrors(t *testing.T) {
	dir := t.TempDir()
	pluginDir := mustMkdir(t, filepath.Join(dir, "ghost"))
	body := `{"$schema":"x","name":"ghost","extensions":{"io.github.fagerbergj.quack":{"schemaVersion":1,` +
		`"modules":[{"name":"ghost","path":"github.com/fagerbergj/quack-extensions/ghost"}]}}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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
plugins:
  seed:
    - ` + pluginDir + `
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newServerValidateCmd()
	c.SilenceUsage = true
	c.SetArgs([]string{cfgPath})
	if err := c.Execute(); err == nil {
		t.Fatal("expected an error for a plugins.seed row declaring an unlinked module")
	}
}

// A plugin workflow shape naming an unconfigured agent fails
// SeedPluginAgentsAndShapes, and `server validate` must surface that error.
func TestServerValidate_PluginShapeMissingAgentErrors(t *testing.T) {
	dir := t.TempDir()
	pluginDir := mustMkdir(t, filepath.Join(dir, "acme"))
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"$schema":"x","name":"acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workflowsDir := mustMkdir(t, filepath.Join(pluginDir, "workflows"))
	shape := "name: acme-job\ntrigger: \"Run the acme job\"\nagents: [ghost]\nshape: \"ONE `ghost` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}
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
plugins:
  seed:
    - ` + pluginDir + `
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newServerValidateCmd()
	c.SilenceUsage = true
	c.SetArgs([]string{cfgPath})
	if err := c.Execute(); err == nil {
		t.Fatal("expected an error for a plugin shape naming a missing agent")
	}
}

func mustMkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
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
