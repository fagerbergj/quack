package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

func loadConfigForTest(path string) (*config.Config, error) {
	return config.Load(path)
}

func TestRegistryRoundTrip(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddServer("local", "http://localhost:8080"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddServer("prod", "https://quack.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Use("prod"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	c2, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c2.Active != "prod" || len(c2.Servers) != 2 || c2.Servers["prod"].URL != "https://quack.example.com" {
		t.Errorf("reload = %+v, want active=prod 2 servers", c2)
	}
	if got := c2.ActiveURL(""); got != "https://quack.example.com" {
		t.Errorf("ActiveURL = %q, want prod url", got)
	}
	if got := c2.ActiveURL("http://override"); got != "http://override" {
		t.Errorf("ActiveURL override = %q", got)
	}

	// Remove clears it and deactivates.
	c2.RemoveServer("prod")
	if c2.Active != "" || len(c2.Servers) != 1 {
		t.Errorf("after remove: active=%q servers=%d", c2.Active, len(c2.Servers))
	}
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}
	// Empty registry (after removing the last server + saving) → "" - the signal
	// to run the duck locally in-process rather than dial a remote.
	c2.RemoveServer("local")
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}
	c3, _ := LoadClient()
	if got := c3.ActiveURL(""); got != "" {
		t.Errorf("empty registry ActiveURL = %q, want \"\" (run-local signal)", got)
	}
}

func TestRegistryAddDuplicate(t *testing.T) {
	c := &ClientConfig{Servers: map[string]ServerRef{}}
	if err := c.AddServer("x", "http://x"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddServer("x", "http://y"); err == nil {
		t.Error("AddServer should reject a duplicate name")
	}
	if err := c.Use("nope"); err == nil {
		t.Error("Use should reject an unregistered server")
	}
}

func TestLoadClientAbsent(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	c, err := LoadClient()
	if err != nil || c == nil || len(c.Servers) != 0 {
		t.Fatalf("LoadClient absent = %+v err=%v, want empty non-nil", c, err)
	}
	// configPath under the temp dir - proves the fallback path resolves.
	if _, err := os.Stat(configPath()); err == nil {
		t.Error("config file should not be created by LoadClient")
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth = %q, want Bearer secret", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"data":[{"id":"qwen3.6-35b"},{"id":"gemma4-26b-a4b"},{"id":"qwen3-embed"}]}`))
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), srv.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gemma4-26b-a4b", "qwen3-embed", "qwen3.6-35b"}
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("ListModels = %v, want %v (sorted)", got, want)
	}
}

func TestListModelsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := ListModels(context.Background(), srv.URL, "bad"); err == nil {
		t.Error("ListModels should error on a non-200 response")
	}
}

func TestPrefillFromEnv(t *testing.T) {
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"},
		{"QUACK_ORCH_MODEL", "orch"}, {"QUACK_JUDGE_MODEL", "judge"}, {"QUACK_EMBED_MODEL", "embed"},
		{"QUACK_IMAGE_MODEL", "vl"}, {"QUACK_MEDIA_MODEL", "omni"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	var a InitAnswers
	PrefillFromEnv(&a)
	if a.Endpoint != "http://x/v1" || a.APIKey != "k" || a.MainModel != "orch" ||
		a.JudgeModel != "judge" || a.EmbedModel != "embed" || a.VisionModel != "vl" || a.AudioModel != "omni" {
		t.Errorf("PrefillFromEnv = %+v, want all fields from env", a)
	}
	// QUACK_RESEARCHER_MODEL fallback when QUACK_ORCH_MODEL is unset.
	t.Setenv("QUACK_ORCH_MODEL", "")
	var b InitAnswers
	t.Setenv("QUACK_RESEARCHER_MODEL", "researcher")
	PrefillFromEnv(&b)
	if b.MainModel != "researcher" {
		t.Errorf("MainModel fallback = %q, want researcher", b.MainModel)
	}
	// An already-set field isn't clobbered.
	var c InitAnswers
	c.MainModel = "preset"
	PrefillFromEnv(&c)
	if c.MainModel != "preset" {
		t.Errorf("PrefillFromEnv clobbered a preset field: %q", c.MainModel)
	}
}

// TestEmitServerConfigRoundTrip proves the generated quack.yaml is valid: it
// round-trips through the real config loader. Guards the wizard's output
// contract (the AGENTS.md spec-driven rule: behavioral drift becomes a failing
// test, not a production incident).
func TestEmitServerConfigRoundTrip(t *testing.T) {
	a := InitAnswers{
		Endpoint:    "http://localhost:11436/v1",
		APIKey:      "k",
		MainModel:   "qwen3.6-35b",
		JudgeModel:  "gemma4-26b-a4b",
		EmbedModel:  "qwen3-embed",
		VisionModel: "qwen3-vl-32b",
		AudioModel:  "qwen3-omni-30b",
		SessionKind: "sqlite",
		MemoryKind:  "sqlite",
		SearchKind:  "exa",
		FetchKind:   "direct",
		WebSearch:   true,
		WebFetch:    true,
	}
	// Only the API key is an env-var ref now; endpoint + models are hardcoded.
	t.Setenv("QUACK_LLM_API_KEY", a.APIKey)

	path := filepath.Join(t.TempDir(), "quack.yaml")
	if err := os.WriteFile(path, []byte(EmitServerConfig(a)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigForTest(path)
	if err != nil {
		t.Fatalf("emitted config failed to load: %v\n---\n%s", err, EmitServerConfig(a))
	}
	// Tailored roster: all four agents when both media models given.
	for _, want := range []string{"web-researcher", "synthesizer", "media-reader", "image-reader"} {
		if _, ok := cfg.Agents[want]; !ok {
			t.Errorf("emitted config missing agent %q", want)
		}
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("server.addr = %q, want :8080", cfg.Server.Addr)
	}
	if cfg.Tools["web_search"].Kind != "exa" || cfg.Tools["web_fetch"].Kind != "direct" {
		t.Errorf("tool kinds = exa/direct? %+v %+v", cfg.Tools["web_search"], cfg.Tools["web_fetch"])
	}
	// Endpoint + models are hardcoded, not env refs.
	if cfg.Providers["default"].Endpoint != a.Endpoint {
		t.Errorf("endpoint = %q, want %q hardcoded", cfg.Providers["default"].Endpoint, a.Endpoint)
	}
}

// TestEmitServerConfigTextOnly: a text-only setup (no vision/audio) gets a lean
// roster - no media-reader, no image-reader.
func TestEmitServerConfigTextOnly(t *testing.T) {
	a := InitAnswers{
		Endpoint: "http://x/v1", MainModel: "m", SessionKind: "sqlite",
		WebSearch: true, WebFetch: true, SearchKind: "exa", FetchKind: "direct",
	}
	t.Setenv("QUACK_LLM_API_KEY", "k")
	path := filepath.Join(t.TempDir(), "quack.yaml")
	if err := os.WriteFile(path, []byte(EmitServerConfig(a)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigForTest(path)
	if err != nil {
		t.Fatalf("emitted text-only config failed: %v", err)
	}
	if _, ok := cfg.Agents["media-reader"]; ok {
		t.Error("text-only setup should not emit media-reader")
	}
	if _, ok := cfg.Agents["image-reader"]; ok {
		t.Error("text-only setup should not emit image-reader")
	}
	if _, ok := cfg.Agents["web-researcher"]; !ok {
		t.Error("text-only setup should still emit web-researcher")
	}
	// No embedder ⇒ no gates judge-less memory; memory off, gates off.
	if cfg.Gates.Enabled() {
		t.Error("text-only (no judge) should disable gates")
	}
}

// TestEmitFillsBlankBackendURL: a blank URL for a kind that needs one is filled
// with that kind's DefaultBackendURL; a kind that needs none (exa) stays bare.
func TestEmitFillsBlankBackendURL(t *testing.T) {
	a := InitAnswers{
		Endpoint: "http://x/v1", MainModel: "m",
		SessionKind: "postgres",                  // blank URL → default DSN
		WebSearch:   true, SearchKind: "searxng", // blank URL → http://localhost:8080
		WebFetch: true, FetchKind: "crawl4ai", // blank URL → http://localhost:11235
	}
	t.Setenv("QUACK_LLM_API_KEY", "k")
	path := filepath.Join(t.TempDir(), "quack.yaml")
	if err := os.WriteFile(path, []byte(EmitServerConfig(a)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigForTest(path)
	if err != nil {
		t.Fatalf("emitted config failed to load: %v\n---\n%s", err, EmitServerConfig(a))
	}
	if got := cfg.Tools["web_search"].URL; got != "http://localhost:8080" {
		t.Errorf("searxng blank URL = %q, want the localhost default", got)
	}
	if got := cfg.Tools["web_fetch"].URL; got != "http://localhost:11235" {
		t.Errorf("crawl4ai blank URL = %q, want the localhost default", got)
	}
	if got := cfg.Stores["default_postgres"].URL; got == "" {
		t.Error("postgres blank URL should be filled with the default DSN")
	}
}

// TestEmitServerConfigCoding: the coding feature emits the three coding agents
// as ACP workers (no tools: list - quack has no native repo/exec tools), the
// workspace section the ACP children run inside, and loads through the real
// config loader.
func TestEmitServerConfigCoding(t *testing.T) {
	t.Setenv("QUACK_LLM_API_KEY", "k")
	base := InitAnswers{
		Endpoint: "http://x/v1", MainModel: "m", SessionKind: "sqlite",
		Coding: true, Sandbox: "none",
	}

	t.Run("with judge", func(t *testing.T) {
		a := base
		a.JudgeModel = "j"
		a.CoderModel = "coder-x"
		path := filepath.Join(t.TempDir(), "quack.yaml")
		if err := os.WriteFile(path, []byte(EmitServerConfig(a)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigForTest(path)
		if err != nil {
			t.Fatalf("emitted coding config failed to load: %v\n---\n%s", err, EmitServerConfig(a))
		}
		for _, want := range []string{"code-implementer", "code-explorer", "code-reviewer"} {
			ag, ok := cfg.Agents[want]
			if !ok {
				t.Fatalf("emitted config missing agent %q", want)
			}
			if ag.Model != "coder-x" {
				t.Errorf("%s model = %q, want coder-x", want, ag.Model)
			}
			if ag.Acp == nil || len(ag.Acp.Command) == 0 {
				t.Errorf("%s must be an ACP agent (coding tools are not in the registry)", want)
			}
			if len(ag.Tools) != 0 {
				t.Errorf("%s tools = %v, want none: an ACP worker builds no quack tools", want, ag.Tools)
			}
		}
		if !cfg.Agents["code-explorer"].Acp.ReadOnly || !cfg.Agents["code-reviewer"].Acp.ReadOnly {
			t.Error("explorer and reviewer must be read_only ACP agents")
		}
		if cfg.Agents["code-implementer"].Acp.ReadOnly {
			t.Error("the implementer must not be read_only")
		}
		if cfg.Agents["code-implementer"].JudgeRounds != 8 {
			t.Errorf("implementer judge_rounds = %d, want 8", cfg.Agents["code-implementer"].JudgeRounds)
		}
		if cfg.Workspace.Sandbox != "none" {
			t.Errorf("workspace.sandbox = %q, want none", cfg.Workspace.Sandbox)
		}
	})

	t.Run("blank coder model reuses main", func(t *testing.T) {
		a := base
		path := filepath.Join(t.TempDir(), "quack.yaml")
		if err := os.WriteFile(path, []byte(EmitServerConfig(a)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigForTest(path)
		if err != nil {
			t.Fatalf("emitted coding config failed to load: %v\n---\n%s", err, EmitServerConfig(a))
		}
		if got := cfg.Agents["code-implementer"].Model; got != "m" {
			t.Errorf("blank coder model should reuse main, got %q", got)
		}
	})

	t.Run("off by default", func(t *testing.T) {
		a := base
		a.Coding = false
		out := EmitServerConfig(a)
		if strings.Contains(out, "code-implementer") || strings.Contains(out, "workspace:") {
			t.Error("coding off should emit no coding agents or workspace section")
		}
	})
}
