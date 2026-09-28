package serve

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg"
)

// sleeperSeed is config/quack.yaml's shape of the sleeper entry, at a fixture tag.
const sleeperSeed = "github:fagerbergj/quack-extensions@sleeper/v9.9.9#sleeper/plugin"

// oldSleeperRow is the row every pre-registry-plugin deployment has on disk.
var oldSleeperRow = pluginreg.Plugin{Name: "sleeper", Source: pluginreg.SourceLocal, Entry: ".agents/plugins/sleeper"}

// sleeperPluginRemote serves the fixture plugin at quack-extensions' layout
// (sleeper/plugin) from a local bare repo, tagged sleeper/v9.9.9.
func sleeperPluginRemote(t *testing.T) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "quack-extensions.git")
	run(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	if err := os.CopyFS(filepath.Join(work, "sleeper", "plugin"), os.DirFS(sleeperPluginRoot)); err != nil {
		t.Fatal(err)
	}
	run(t, work, "init", "--quiet", "--initial-branch=main")
	run(t, work, "add", ".")
	run(t, work, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", "sleeper plugin")
	run(t, work, "tag", "sleeper/v9.9.9")
	run(t, work, "push", "--quiet", bare, "main", "sleeper/v9.9.9")
	prev := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string {
		if owner == "fagerbergj" && repo == "quack-extensions" {
			return bare
		}
		return filepath.Join(t.TempDir(), "unreachable.git")
	}
	t.Cleanup(func() { pluginreg.RemoteURL = prev })
}

// Seeding owns local rows and rows it created: each follows a changed seed
// entry on either backend. A REST-owned row keeps its entry; one predating the flag
// that still matches its seed is marked seeded.
func TestSeedRegistryFollowsChangedEntriesOfRowsItOwns(t *testing.T) {
	dbReg := func(t *testing.T) pluginreg.FetchRegistry {
		db, err := pluginreg.OpenDB("sqlite", filepath.Join(t.TempDir(), "plugins.db"))
		if err != nil {
			t.Fatal(err)
		}
		reg, err := pluginreg.NewDBRegistry(db, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}
	fsReg := func(t *testing.T) pluginreg.FetchRegistry { return pluginreg.NewFSRegistry(t.TempDir()) }
	gh := func(name, entry string, seeded bool) pluginreg.Plugin {
		e, err := pluginreg.ParseEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		p := pluginreg.FromEntry(e)
		p.SHA, p.Seeded = "abc123", seeded
		return p
	}
	for name, newReg := range map[string]func(*testing.T) pluginreg.FetchRegistry{"fs": fsReg, "db": dbReg} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reg := newReg(t)
			rows := []pluginreg.Plugin{
				oldSleeperRow,
				gh("ponytail", "github:DietrichGebert/ponytail@v1", true),
				gh("widgets", "github:acme/widgets@v1", false),
				gh("dotagents", "github:fagerbergj/dotagents", false),
			}
			for _, p := range rows {
				if err := reg.Put(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			seed := []string{sleeperSeed, "github:DietrichGebert/ponytail@v2", "github:acme/widgets@v2", "github:fagerbergj/dotagents"}
			if err := seedRegistry(ctx, reg, seed); err != nil {
				t.Fatal(err)
			}
			got := map[string]pluginreg.Plugin{}
			listed, err := reg.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range listed {
				got[r.Name] = r
			}
			check := func(name, entry, ref string, seeded bool) {
				r := got[name]
				if r.Entry != entry || r.Ref != ref || r.Seeded != seeded {
					t.Errorf("%s = %+v, want entry %q ref %q seeded %v", name, r, entry, ref, seeded)
				}
			}
			check("sleeper", sleeperSeed, "sleeper/v9.9.9", true)
			check("ponytail", "github:DietrichGebert/ponytail@v2", "v2", true)
			check("widgets", "github:acme/widgets@v1", "v1", false)
			check("dotagents", "github:fagerbergj/dotagents", "", true)
			if got["ponytail"].SHA != "abc123" {
				t.Errorf("ponytail sha = %q, want the old clone's sha kept until the fetch moves it", got["ponytail"].SHA)
			}
		})
	}
}

// End to end: a deployment booting with the old local sleeper row and the
// new github seed fetches the plugin and builds its agents and workflows.
func TestBootMigratesLocalSleeperRowToGithubPlugin(t *testing.T) {
	sleeperPluginRemote(t)
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	cfg := reloadTestConfig()
	enableSleeperExtension(t, cfg)
	rig := newReloadRig(t, cfg, realBuild(t))
	for _, tl := range newSleeperExtension(t).Tools() {
		rig.r.roster.sdkTools = append(rig.r.roster.sdkTools, extTool{provider: "sleeper", tool: tl})
	}
	cfg.Plugins.Seed = []string{sleeperSeed}
	ctx := context.Background()
	if err := rig.reg.Put(ctx, oldSleeperRow); err != nil {
		t.Fatal(err)
	}

	_, rows, err := (&boot{cfg: cfg}).bootPluginRegistry(ctx, nil)
	if err != nil {
		t.Fatalf("bootPluginRegistry: %v", err)
	}
	row := rows[0]
	if row.Source != pluginreg.SourceGitHub || row.Ref != "sleeper/v9.9.9" || row.SHA == "" || row.Error != "" {
		t.Fatalf("sleeper row = %+v, want the github row fetched at sleeper/v9.9.9", row)
	}

	rep := rig.reload(t)
	if len(rep.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", rep.Failures)
	}
	if !slices.Equal(rep.Agents.Added, sleeperAgentNames) || !slices.Equal(rep.Workflows.Added, sleeperWorkflowShapeNames) {
		t.Fatalf("added agents %v, workflows %v; want %v, %v", rep.Agents.Added, rep.Workflows.Added, sleeperAgentNames, sleeperWorkflowShapeNames)
	}
}

// A seed ref bump on a seeded row checks the new tag out at the next boot; a
// bump to a missing tag keeps serving the old checkout and records the error.
func TestBootFetchFollowsSeedRefBump(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "widgets.git")
	run(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	run(t, work, "init", "--quiet", "--initial-branch=main")
	for _, tag := range []string{"v1", "v2"} {
		if err := os.WriteFile(filepath.Join(work, "plugin.json"), []byte(`{"name":"widgets","version":"`+tag+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, work, "add", ".")
		run(t, work, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", tag)
		run(t, work, "tag", tag)
	}
	run(t, work, "push", "--quiet", bare, "main", "v1", "v2")
	prev := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(string, string) string { return bare }
	t.Cleanup(func() { pluginreg.RemoteURL = prev })

	ctx := context.Background()
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	boot := func(entry string) pluginreg.Plugin {
		t.Helper()
		if err := seedRegistry(ctx, reg, []string{entry}); err != nil {
			t.Fatal(err)
		}
		rows, err := reg.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return fetchRegistryPlugins(ctx, reg, rows)[0]
	}
	head := func() string {
		return strings.TrimSpace(run(t, pluginreg.CloneDir(root, "widgets"), "rev-parse", "HEAD"))
	}
	v2 := strings.TrimSpace(run(t, work, "rev-parse", "v2"))

	boot("github:acme/widgets@v1")
	if row := boot("github:acme/widgets@v2"); row.SHA != v2 || row.Error != "" || head() != v2 {
		t.Fatalf("after bump to v2: row %+v, HEAD %s; want both at %s", row, head(), v2)
	}
	row := boot("github:acme/widgets@v3")
	if row.Ref != "v3" || row.SHA != v2 || row.Error == "" || head() != v2 {
		t.Fatalf("after bump to missing v3: row %+v, HEAD %s; want ref v3, sha and HEAD still %s, error set", row, head(), v2)
	}
	if listed, _ := reg.List(ctx); listed[0].SHA != v2 || listed[0].Error == "" {
		t.Fatalf("persisted row %+v, want the v2 sha and the fetch error", listed[0])
	}
}
