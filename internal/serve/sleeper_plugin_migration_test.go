package serve

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// A local row is config-owned, so a changed seed entry replaces it on either
// backend; an unchanged local row, and any github row, is left alone.
func TestSeedRegistryReplacesLocalRowWhoseEntryChanged(t *testing.T) {
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
	for name, newReg := range map[string]func(*testing.T) pluginreg.FetchRegistry{"fs": fsReg, "db": dbReg} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reg := newReg(t)
			pinned := pluginreg.Plugin{Name: "ponytail", Source: pluginreg.SourceGitHub, Entry: "github:DietrichGebert/ponytail@v1", Owner: "DietrichGebert", Repo: "ponytail", Ref: "v1"}
			usage := pluginreg.Plugin{Name: "usage", Source: pluginreg.SourceLocal, Entry: ".agents/plugins/usage"}
			for _, p := range []pluginreg.Plugin{oldSleeperRow, pinned, usage} {
				if err := reg.Put(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			seed := []string{sleeperSeed, "github:DietrichGebert/ponytail@v2", ".agents/plugins/usage"}
			if err := seedRegistry(ctx, reg, seed); err != nil {
				t.Fatal(err)
			}
			rows, err := reg.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, r := range rows {
				got[r.Name] = r.Entry
			}
			want := map[string]string{"sleeper": sleeperSeed, "ponytail": pinned.Entry, "usage": usage.Entry}
			if !maps.Equal(got, want) {
				t.Fatalf("rows = %v, want %v", got, want)
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
