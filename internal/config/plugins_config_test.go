package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A plugins: block that sets root but omits seed: still falls back to the default roots
// (Seed nil is "omitted", []string{} is "explicit empty" - see PluginsConfig.UnmarshalYAML).
func TestLoadPluginsBlockOmittedSeedUsesDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
plugins:
  root: /custom/plugins/root
`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Plugins.Seed
	if len(got) != len(defaultSkillPlugins) {
		t.Fatalf("Plugins.Seed = %v, want the defaults %v", got, defaultSkillPlugins)
	}
	for i, want := range defaultSkillPlugins {
		if got[i] != want {
			t.Fatalf("Plugins.Seed = %v, want the defaults %v", got, defaultSkillPlugins)
		}
	}
	if c.Plugins.Root != "/custom/plugins/root" {
		t.Fatalf("plugins.root = %q, want /custom/plugins/root", c.Plugins.Root)
	}
}

func TestLoadPluginsBlockExplicitEmptySeedStaysEmpty(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
plugins:
  seed: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Plugins.Seed; len(got) != 0 {
		t.Fatalf("Plugins.Seed = %v, want empty (explicit seed: [])", got)
	}
}

// The block form's custom UnmarshalYAML bypasses KnownFields(true), so it must reject unknown keys itself.
func TestLoadPluginsBlockRejectsUnknownField(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
plugins:
  rooot: /typo
`))
	if err == nil {
		t.Fatal("expected an error for the unknown field plugins.rooot")
	}
}

func TestLoadPluginsRejectsUndefinedStore(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
plugins:
  store: nope
`))
	if err == nil {
		t.Fatal("expected an error: plugins.store names no stores[] entry")
	}
}

// TestLoadPluginsRejectsNonDBStoreKind: plugins.store must be sqlite or
// postgres - a qdrant/langfuse store makes no sense as a row store.
func TestLoadPluginsRejectsNonDBStoreKind(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  vec: { kind: qdrant, url: http://q }
session: { store: main }
orchestrator: { provider: default, model: m }
plugins:
  store: vec
`))
	if err == nil {
		t.Fatal("expected an error: plugins.store kind qdrant is not sqlite or postgres")
	}
}

// TestLoadPluginsAcceptsPostgresStore: plugins.store naming a postgres
// stores[] entry (e.g. the same one session.store uses) is valid config.
func TestLoadPluginsAcceptsPostgresStore(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
plugins:
  store: main
`))
	if err != nil {
		t.Fatalf("plugins.store: main (postgres, same as session.store) should be valid: %v", err)
	}
	if c.Plugins.Store != "main" {
		t.Fatalf("Plugins.Store = %q, want main", c.Plugins.Store)
	}
}

// TestLoadPluginsAcceptsSqliteStore: plugins.store naming a sqlite
// stores[] entry is valid config.
func TestLoadPluginsAcceptsSqliteStore(t *testing.T) {
	c, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  plugindb: { kind: sqlite, url: /tmp/plugins.db }
session: { store: main }
orchestrator: { provider: default, model: m }
plugins:
  store: plugindb
`))
	if err != nil {
		t.Fatalf("plugins.store: plugindb (sqlite) should be valid: %v", err)
	}
	if c.Plugins.Store != "plugindb" {
		t.Fatalf("Plugins.Store = %q, want plugindb", c.Plugins.Store)
	}
}

func TestLoadPluginsRejectsMalformedSeedEntry(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
plugins:
  seed:
    - "github:owner/repo#/etc/passwd"
`))
	if err == nil {
		t.Fatal("expected an error for a seed entry whose #path escapes the plugin root")
	}
}

// A plugins: block with no seed: falls through to skills.plugins, so the "skills.plugins is ignored"
// warning must not fire.
func TestLoadPluginsBlockNoSeedFallsBackToSkillsPluginsWithoutWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c, err := Load(writeTemp(t, baseConfig+`
skills:
  plugins:
    - .agents/local/dotagents
plugins:
  root: /custom/plugins/root
`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Plugins.Seed
	if len(got) != 1 || got[0] != ".agents/local/dotagents" {
		t.Fatalf("Plugins.Seed = %v, want skills.plugins to be used", got)
	}
	if strings.Contains(buf.String(), "skills.plugins is ignored") {
		t.Fatalf("skills.plugins was actually used but the log says it was ignored:\n%s", buf.String())
	}
}

// A degenerate local root like "/" fails config load via ParseEntry, not as a later registry error.
func TestLoadPluginsRejectsDegenerateLocalSeed(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
plugins:
  seed: ["/"]
`))
	if err == nil {
		t.Fatal("expected an error for the degenerate local seed entry \"/\"")
	}
}

func TestLoadPluginsBareListStillMeansSeed(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig+`
plugins:
  - .agents/local/dotagents
  - github:fagerbergj/ponytail@v1.4
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".agents/local/dotagents", "github:fagerbergj/ponytail@v1.4"}
	got := c.Plugins.Seed
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Plugins.Seed = %v, want %v (bare list means seed:, local and github: entries alike)", got, want)
	}
}

// plugins.store naming a stores[] entry with no url fails load: an empty sqlite DSN would create
// a db file literally named "?_pragma=..." in the CWD.
func TestLoadPluginsRejectsEmptyStoreURL(t *testing.T) {
	_, err := Load(writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://x }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: postgres, url: u }
  plugindb: { kind: sqlite }
session: { store: main }
orchestrator: { provider: default, model: m }
plugins:
  store: plugindb
`))
	if err == nil {
		t.Fatal("expected an error: plugins.store plugindb has an empty url")
	}
}
