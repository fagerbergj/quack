package serve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/store"
)

// TestOpenPluginRegistryFilesystemWhenStoreUnset: plugins.store == "" must
// still resolve to *pluginreg.FSRegistry (unchanged, #1427 P0/P1 behavior).
func TestOpenPluginRegistryFilesystemWhenStoreUnset(t *testing.T) {
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: t.TempDir()}}}
	reg, err := b.openPluginRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.(*pluginreg.FSRegistry); !ok {
		t.Fatalf("openPluginRegistry() = %T, want *pluginreg.FSRegistry", reg)
	}
}

// TestOpenPluginRegistrySqliteDifferentStoreOpensOwnDB: plugins.store naming
// a sqlite stores[] entry that ISN'T session.store opens its own connection
// (via pluginreg.OpenDB) and round-trips Put/List through it.
func TestOpenPluginRegistrySqliteDifferentStoreOpensOwnDB(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "plugins.db")
	b := &boot{cfg: &config.Config{
		Plugins: &config.PluginsConfig{Root: root, Store: "plugindb"},
		Stores:  map[string]config.StoreConfig{"plugindb": {Kind: "sqlite", URL: dbPath}},
		Session: config.SessionConfig{Store: "main"}, // deliberately NOT "plugindb"
	}}
	reg, err := b.openPluginRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.(*pluginreg.DBRegistry); !ok {
		t.Fatalf("openPluginRegistry() = %T, want *pluginreg.DBRegistry", reg)
	}
	ctx := context.Background()
	if err := reg.Put(ctx, pluginreg.Plugin{Name: "widgets", Source: pluginreg.SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatal(err)
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "widgets" {
		t.Fatalf("List() = %+v, want one row named widgets", list)
	}
}

// TestOpenPluginRegistryReusesSessionStoreConnection: plugins.store naming
// session.store's own name reuses st.DB() - proved by reading a Put row
// back through st.DB() directly, not just through the returned registry.
func TestOpenPluginRegistryReusesSessionStoreConnection(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := store.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	b := &boot{cfg: &config.Config{
		Plugins: &config.PluginsConfig{Root: root, Store: "main"},
		Stores:  map[string]config.StoreConfig{"main": {Kind: "sqlite", URL: dbPath}},
		Session: config.SessionConfig{Store: "main"},
	}}
	reg, err := b.openPluginRegistry(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(context.Background(), pluginreg.Plugin{Name: "widgets", Source: pluginreg.SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.DB().Raw(`SELECT name FROM plugin_rows WHERE name = ?`, "widgets").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	if name != "widgets" {
		t.Fatalf("plugin_rows.name via st.DB() = %q, want widgets (registry must have reused the session store's own connection)", name)
	}
}
