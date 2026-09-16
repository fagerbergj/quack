package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestLoadPluginsBlockOmittedSeedUsesDefaults covers issue #13: a plugins:
// block that sets root but omits seed: must still fall back to the default
// plugin roots, not silently drop every default (Seed == nil is "omitted",
// Seed == []string{} is "explicit empty" - see PluginsConfig.UnmarshalYAML).
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

// TestLoadPluginsBlockRejectsUnknownField covers issue #14: the custom
// UnmarshalYAML for the block form bypasses the decoder's KnownFields(true),
// so it must reject unrecognized keys itself.
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
// postgres (P3) - a qdrant/langfuse store makes no sense as a row store.
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

// TestLoadPluginsBlockNoSeedFallsBackToSkillsPluginsWithoutWarning: a
// plugins: block with no seed: key still falls through to skills.plugins
// (issue #13's fix), so the "skills.plugins is ignored" warning must not
// fire in that case - it would actually be used.
func TestLoadPluginsBlockNoSeedFallsBackToSkillsPluginsWithoutWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c, err := Load(writeTemp(t, baseConfig+`
skills:
  plugins:
    - .agents/vendor/dotagents
plugins:
  root: /custom/plugins/root
`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Plugins.Seed
	if len(got) != 1 || got[0] != ".agents/vendor/dotagents" {
		t.Fatalf("Plugins.Seed = %v, want skills.plugins to be used", got)
	}
	if strings.Contains(buf.String(), "skills.plugins is ignored") {
		t.Fatalf("skills.plugins was actually used but the log says it was ignored:\n%s", buf.String())
	}
}

// TestLoadPluginsRejectsDegenerateLocalSeed: a degenerate local root like "/"
// must fail config load via ParseEntry, not surface later as an opaque
// registry-put error.
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
  - .agents/vendor/dotagents
  - github:fagerbergj/ponytail@v1.4
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".agents/vendor/dotagents", "github:fagerbergj/ponytail@v1.4"}
	got := c.Plugins.Seed
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Plugins.Seed = %v, want %v (bare list means seed:, local and github: entries alike)", got, want)
	}
}

// TestLoadPluginsRejectsEmptyStoreURL is the adversarial-review S2
// regression: plugins.store naming a stores[] entry with no url must fail
// load, not silently reach pluginreg.OpenDB with an empty DSN (which, for
// sqlite, creates a db file literally named "?_pragma=..." in the CWD).
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
