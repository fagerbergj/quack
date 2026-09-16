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
	got := c.PluginRoots()
	if len(got) != len(defaultSkillPlugins) {
		t.Fatalf("PluginRoots() = %v, want the defaults %v", got, defaultSkillPlugins)
	}
	for i, want := range defaultSkillPlugins {
		if got[i] != want {
			t.Fatalf("PluginRoots() = %v, want the defaults %v", got, defaultSkillPlugins)
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
	if got := c.PluginRoots(); len(got) != 0 {
		t.Fatalf("PluginRoots() = %v, want empty (explicit seed: [])", got)
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

func TestLoadPluginsRejectsNonFilesystemStore(t *testing.T) {
	_, err := Load(writeTemp(t, baseConfig+`
plugins:
  store: default_postgres
`))
	if err == nil {
		t.Fatal("expected an error: plugins.store is filesystem-only until P3")
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
	got := c.PluginRoots()
	if len(got) != 1 || got[0] != ".agents/vendor/dotagents" {
		t.Fatalf("PluginRoots() = %v, want skills.plugins to be used", got)
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
	got := c.PluginRoots()
	if len(got) != 1 || got[0] != ".agents/vendor/dotagents" {
		t.Fatalf("PluginRoots() = %v, want just the local entry", got)
	}
}
