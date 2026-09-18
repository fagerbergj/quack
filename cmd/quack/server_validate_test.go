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

// A configured agent whose bundle: directory is missing (a stale or typo'd
// path - e.g. left behind by a plugin migration) is reported, not silently
// accepted; validate still reports overall status ok, since boot's own
// optional-drop/failure handling is the real gate, not this preview.
func TestServerValidate_StaleAgentBundlePathReported(t *testing.T) {
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
  ghost-agent:
    bundle: ` + filepath.Join(dir, "does-not-exist") + `
    provider: default
    model: m
    optional: true
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
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok (this is a warning, not a boot error)", got.Status)
	}
	if len(got.StaleBundles) != 1 || got.StaleBundles[0] != "ghost-agent" {
		t.Fatalf("StaleBundles = %v, want [ghost-agent]", got.StaleBundles)
	}
}

// `server validate` must stay read-only and offline: validating the real
// shipped config/quack.yaml (plugins.seed includes two github: entries) in a
// clean temp workspace must create no files and touch no network - a
// github: entry with no local clone yet is reported unresolvable, never fetched.
func TestServerValidate_OfflineNoNetworkNoFileWrites(t *testing.T) {
	wsRoot := filepath.Join(t.TempDir(), "workspace")
	for k, v := range map[string]string{
		"QUACK_LLM_ENDPOINT": "http://localhost:1", "QUACK_LLM_API_KEY": "x",
		"QUACK_RESEARCHER_MODEL": "qwen3.8-27b", "QUACK_CODER_MODEL": "qwen3.8-27b",
		"QUACK_JUDGE_MODEL": "gemma4-26b-a4b", "QUACK_MEDIA_MODEL": "qwen3-omni-30b", "QUACK_IMAGE_MODEL": "qwen3-vl-32b",
		"QUACK_EMBED_MODEL": "qwen3-embed", "QUACK_ORCH_MODEL": "qwen3.8-27b",
		"QUACK_SEARXNG_URL": "http://s", "QUACK_CRAWL4AI_URL": "http://c", "QUACK_EXA_API_KEY": "x",
		"QUACK_DATABASE_URL": "postgres://localhost/db", "QUACK_WORKSPACE_ROOT": wsRoot,
	} {
		t.Setenv(k, v)
	}

	var out bytes.Buffer
	c := newServerValidateCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"../../config/quack.yaml", "--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("server validate: %v\noutput:\n%s", err, out.String())
	}

	var got serverValidateResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	for _, want := range []string{"dotagents", "ponytail"} {
		found := false
		for _, name := range got.Unresolvable {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Unresolvable = %v, want %q (never cloned in this clean workspace)", got.Unresolvable, want)
		}
	}

	if _, err := os.Stat(wsRoot); err == nil {
		t.Errorf("workspace root %q was created - validate must not touch disk", wsRoot)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
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
