package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
)

// run runs a git command against dir, failing the test on error - mirrors
// pluginreg's own (unexported, package-private) fixture helper.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newFixtureRepo makes a bare "remote" repo plus a pushing work tree under
// t.TempDir() - the fetch target in place of github.com (#1427 F1: no test
// may touch the network).
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)

	work := t.TempDir()
	run(t, work, "init", "--quiet", "--initial-branch=main")
	run(t, work, "config", "user.email", "test@example.com")
	run(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "skills"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "v1")
	run(t, work, "remote", "add", "origin", bare)
	run(t, work, "push", "--quiet", "origin", "main")
	return bare
}

// TestBootPluginRegistrySeedsFetchesAndSkipsEmbedded is the P1 boot-seeding
// verification against local fixtures only (#1427 F1): a reachable row gets
// a sha, an unreachable one (still a local path - no network) stores an
// error and boot continues, a local entry is never fetched. Folds in the old
// registryPluginRoots-skips-embedded assertion as one on-disk check: every
// seeded row gets entry.json, the in-memory embedded quack row never does.
func TestBootPluginRegistrySeedsFetchesAndSkipsEmbedded(t *testing.T) {
	bare := newFixtureRepo(t)
	prev := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string {
		if owner == "acme" && repo == "widgets" {
			return bare
		}
		return filepath.Join(t.TempDir(), "does-not-exist.git") // local, unreachable - never touches the network
	}
	t.Cleanup(func() { pluginreg.RemoteURL = prev })

	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	ctx := context.Background()
	seed := []string{"github:acme/widgets", "github:acme/unreachable", ".agents/local/dotagents"}
	if err := seedRegistry(ctx, reg, seed); err != nil {
		t.Fatal(err)
	}
	rows, err := reg.List(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("List() = %+v, err %v, want 3 rows", rows, err)
	}

	fetched := fetchRegistryPlugins(ctx, reg, rows)
	if len(fetched) != 3 {
		t.Fatalf("fetchRegistryPlugins returned %d rows, want 3 (boot continues past the failure)", len(fetched))
	}
	byName := map[string]pluginreg.Plugin{}
	for _, p := range fetched {
		byName[p.Name] = p
	}
	if byName["widgets"].SHA == "" || byName["widgets"].Error != "" {
		t.Errorf("widgets (reachable) = %+v, want a sha and no error", byName["widgets"])
	}
	if byName["unreachable"].Error == "" {
		t.Errorf("unreachable = %+v, want an error recorded", byName["unreachable"])
	}
	if byName["dotagents"].SHA != "" || byName["dotagents"].Error != "" {
		t.Errorf("local entry %+v: local entries are never fetched", byName["dotagents"])
	}

	for _, name := range []string{"widgets", "unreachable", "dotagents"} {
		if _, err := os.Stat(filepath.Join(root, name, "entry.json")); err != nil {
			t.Errorf("entry.json missing for seeded row %q: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "quack")); err == nil {
		t.Error("the embedded quack row must never be written to disk")
	}
	roots := registryPluginRoots(root, append(fetched, pluginreg.EmbeddedQuackPlugin()))
	if len(roots) != 3 {
		t.Fatalf("registryPluginRoots = %v, want 3 (the embedded row excluded)", roots)
	}
}

// TestSeedRegistryPreservesFetchedState is #1427 F6: re-seeding a name
// already on disk must never reset what a prior fetch recorded.
func TestSeedRegistryPreservesFetchedState(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := reg.Put(ctx, pluginreg.Plugin{
		Name: "widgets", Source: pluginreg.SourceGitHub, Entry: "github:acme/widgets",
		Owner: "acme", Repo: "widgets", SHA: "deadbeef", FetchedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := seedRegistry(ctx, reg, []string{"github:acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	rows, err := reg.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List() = %+v, err %v, want 1 row", rows, err)
	}
	if rows[0].SHA != "deadbeef" {
		t.Fatalf("SHA = %q, want deadbeef (re-seeding must not reset an already-fetched row)", rows[0].SHA)
	}
}

// TestAcpRegistryPluginRefs is #1427 F4/R4: the embedded quack ref (no sha)
// is ALWAYS present, even with an empty registry (it's always in scope), and
// a row literally named "quack" dedupes/wins over the synthetic one.
func TestAcpRegistryPluginRefs(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	ctx := context.Background()
	cfg := &config.Config{Plugins: &config.PluginsConfig{Root: root}}

	empty := acpRegistryPluginRefs(cfg, reg)()
	if len(empty) != 1 || empty[0].Name != "quack" || empty[0].SHA != "" {
		t.Fatalf("acpRegistryPluginRefs() with an empty registry = %+v, want exactly [{quack, \"\"}]", empty)
	}

	if err := reg.Put(ctx, pluginreg.Plugin{Name: "dotagents", Source: pluginreg.SourceLocal, Entry: ".agents/local/dotagents"}); err != nil {
		t.Fatal(err)
	}
	got := acpRegistryPluginRefs(cfg, reg)()
	if len(got) != 2 {
		t.Fatalf("acpRegistryPluginRefs() = %+v, want 2 (dotagents + the synthetic embedded quack)", got)
	}
	var quackSHA string
	var quackCount int
	for _, r := range got {
		if r.Name == "quack" {
			quackCount++
			quackSHA = r.SHA
		}
	}
	if quackCount != 1 || quackSHA != "" {
		t.Fatalf("quack ref: count=%d sha=%q, want exactly 1 with an empty sha", quackCount, quackSHA)
	}

	if err := reg.Put(ctx, pluginreg.Plugin{
		Name: "quack", Source: pluginreg.SourceGitHub, Entry: "github:fagerbergj/quack",
		Owner: "fagerbergj", Repo: "quack", SHA: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	got = acpRegistryPluginRefs(cfg, reg)()
	quackCount = 0
	for _, r := range got {
		if r.Name != "quack" {
			continue
		}
		quackCount++
		if r.SHA != "deadbeef" {
			t.Errorf("quack ref sha = %q, want deadbeef (the registry row wins over the embedded synthetic)", r.SHA)
		}
	}
	if quackCount != 1 {
		t.Fatalf("quack appeared %d times, want exactly 1 (deduped)", quackCount)
	}
}

// TestRegistrySignature is #1427 F5's cache key: stable for the same rows,
// different when a sha (fetch outcome) or the row set changes.
func TestRegistrySignature(t *testing.T) {
	a1 := []pluginreg.Plugin{{Name: "dotagents", SHA: "aaa"}}
	a2 := []pluginreg.Plugin{{Name: "dotagents", SHA: "aaa"}}
	b := []pluginreg.Plugin{{Name: "dotagents", SHA: "bbb"}}
	if registrySignature(a1) != registrySignature(a2) {
		t.Fatal("registrySignature must be stable for equivalent rows")
	}
	if registrySignature(a1) == registrySignature(b) {
		t.Fatal("registrySignature must change when a row's sha changes")
	}
}

// TestAcpRegistryClosuresUseTheDBBackedRegistry is the adversarial-review S1
// regression: acpRegistrySkillPaths/acpRegistryPluginRefs used to always
// build their own pluginreg.NewFSRegistry(cfg.Plugins.Root), so a
// DB-backed plugins.store left ACP children with no skill_paths and no
// plugin provenance - List() against the filesystem root always came back
// empty. Both closures now take the SAME registry boot opened.
func TestAcpRegistryClosuresUseTheDBBackedRegistry(t *testing.T) {
	pluginRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte(`{"name":"extra"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeVendorSkill(t, filepath.Join(pluginRoot, "skills"), "widget-maker", "Makes widgets.")
	entry, err := pluginreg.ParseEntry(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "plugins.db")
	db, err := pluginreg.OpenDB("sqlite", dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	reg, err := pluginreg.NewDBRegistry(db, t.TempDir())
	if err != nil {
		t.Fatalf("NewDBRegistry: %v", err)
	}
	ctx := context.Background()
	if err := reg.Put(ctx, pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}

	// plugins.root here is deliberately a DIFFERENT, empty directory - if
	// either closure fell back to a filesystem registry over cfg.Plugins.Root
	// it would see zero rows, not the one Put above through the DB registry.
	cfg := &config.Config{Plugins: &config.PluginsConfig{Root: t.TempDir()}}

	refs := acpRegistryPluginRefs(cfg, reg)()
	var found bool
	for _, r := range refs {
		if r.Name == entry.Name() {
			found = true
		}
	}
	if !found {
		t.Fatalf("acpRegistryPluginRefs(cfg, dbReg)() = %+v, want a ref named %q", refs, entry.Name())
	}

	paths := acpRegistrySkillPaths(cfg, reg)()
	wantDir := filepath.Join(pluginRoot, "skills")
	found = false
	for _, p := range paths {
		if p == wantDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("acpRegistrySkillPaths(cfg, dbReg)() = %v, want %q among them", paths, wantDir)
	}
}
