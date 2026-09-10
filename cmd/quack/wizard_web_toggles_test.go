package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/serve"
)

// TestEmitServerConfig_WebTogglesBoot: only web_search fails at boot when
// absent from `tools:` - web_fetch defaults to a direct fetcher instead.
func TestEmitServerConfig_WebTogglesBoot(t *testing.T) {
	cases := []struct {
		name                string
		webSearch, webFetch bool
	}{
		{"both off", false, false},
		{"search only", true, false},
		{"fetch only", false, true},
		{"both on", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := cli.InitAnswers{
				Endpoint:    "http://127.0.0.1:1/v1", // never dialed while booting
				APIKey:      "x",
				MainModel:   "m",
				SessionKind: "sqlite",
				SessionURL:  filepath.Join(dir, "store.db"),
				WebSearch:   tc.webSearch,
				WebFetch:    tc.webFetch,
				SearchKind:  "exa",
				FetchKind:   "direct",
				// Coding is what makes EmitServerConfig emit workspace: at
				// all; none matches the other in-process boot tests (no bwrap needed).
				Coding:  true,
				Sandbox: "none",
			}
			t.Setenv("QUACK_LLM_API_KEY", a.APIKey)

			cfgPath := filepath.Join(dir, "quack.yaml")
			rendered := cli.EmitServerConfig(a)
			if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgPath) // the `server validate` path
			if err != nil {
				t.Fatalf("server validate: %v\n---\n%s", err, rendered)
			}
			_, stop, err := serve.InProcessFromConfig(context.Background(), cfg)
			if err != nil {
				t.Fatalf("boot: %v (an agent referencing an undefined web_search/web_fetch tool fails here)\n---\n%s", err, rendered)
			}
			stop()
		})
	}
}
