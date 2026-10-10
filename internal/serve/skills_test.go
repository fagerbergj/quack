package serve

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// initSkills combines resolvePlugins and buildSkillsInit, which buildFromConfig calls separately
// (plugin agents/shapes seed in between); a test convenience only.
func (b *boot) initSkills(ctx context.Context, jail *workspace.Jail, st *store.Store, shapesRef *atomic.Pointer[[]workflowcatalog.Shape]) (skillsInit, error) {
	reg, _, plugins, err := b.resolvePlugins(ctx, st)
	if err != nil {
		return skillsInit{}, err
	}
	return b.buildSkillsInit(jail, reg, plugins, shapesRef)
}

// TestSkillsLoad: every SKILL.md in skills/ and .claude/skills/ passes skilltoolset validation (name,
// 1024-char description cap); a bad one crashes startup or poisons every agent that clones quack.
func TestSkillsLoad(t *testing.T) {
	for _, dir := range []string{"../../skills", "../../.claude/skills", "../../" + dotagentsEmbeddedSkills} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read skills dir %s: %v", dir, err)
		}
		src := skill.NewFileSystemSource(os.DirFS(dir))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := src.LoadFrontmatter(context.Background(), e.Name()); err != nil {
				t.Errorf("skill %s/%s frontmatter failed to load: %v", dir, e.Name(), err)
			}
		}
	}
}

// resolvePlugins composes plugin.Resolve the way boot does - tests want the
// resolved plugins without asserting on the error return.
func resolvePlugins(roots []string) []plugin.Plugin {
	plugins, _ := plugin.Resolve(roots)
	return plugins
}

// writeVendorSkill lays down dir/<name>/SKILL.md in the layout plugin skills/ dirs ship.
func writeVendorSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n\nBody of " + name + ".\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePluginManifest lays down a root plugin.json (Agent Plugins format) so
// a temp dir resolves as a plugin via internal/plugin.
func writePluginManifest(t *testing.T, root, name string) {
	t.Helper()
	body := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"` + name + `"}`
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNewSkillSourceMergesVendoredSkills: a real-shaped plugin root (plugin.json + skills/) merges with the
// shipped skills into one Source, which load_skill("ponytail") in agent prompts depends on.
func TestNewSkillSourceMergesVendoredSkills(t *testing.T) {
	vendor := t.TempDir()
	writePluginManifest(t, vendor, "ponytail")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "ponytail", "Forces the laziest solution that actually works.")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "ponytail-review", "Code review focused exclusively on over-engineering.")

	src := newSkillSource(resolvePlugins([]string{vendor}))
	ctx := context.Background()

	for _, name := range []string{"ponytail:ponytail", "ponytail:ponytail-review"} {
		fm, err := src.LoadFrontmatter(ctx, name)
		if err != nil {
			t.Fatalf("LoadFrontmatter(%q): %v", name, err)
		}
		if fm.Name != name {
			t.Errorf("frontmatter name = %q, want %q", fm.Name, name)
		}
		if _, err := src.LoadInstructions(ctx, name); err != nil {
			t.Errorf("LoadInstructions(%q): %v", name, err)
		}
	}
	// The shipped library still resolves through the merged source as the embedded quack plugin
	// (bundledir falls back to the embedded copy since cwd is this package dir).
	if _, err := src.LoadFrontmatter(ctx, "quack:plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(quack:plan-work) via merged source: %v", err)
	}
}

// TestNewSkillSourceMissingPluginRoot: a configured plugin root missing on disk never fails the run;
// it's absent from the merged source and quack's own skills still resolve.
func TestNewSkillSourceMissingPluginRoot(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	if _, err := src.LoadFrontmatter(context.Background(), "quack:plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(quack:plan-work): %v", err)
	}
}

// TestNewSkillSourceNoPluginsConfigured: quack's skills resolve and so does format-markdown via the embedded
// dotagents copy; ponytail has no such fallback and stays absent.
func TestNewSkillSourceNoPluginsConfigured(t *testing.T) {
	src := newSkillSource(nil)
	ctx := context.Background()
	if _, err := src.LoadFrontmatter(ctx, "quack:plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(quack:plan-work) via quack's own shipped skills: %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "quack:format-markdown"); err != nil {
		t.Errorf("LoadFrontmatter(quack:format-markdown) via embedded dotagents fallback: %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "ponytail:ponytail"); err == nil {
		t.Error("LoadFrontmatter(ponytail:ponytail): want not-found without any plugin roots configured")
	}
}

// TestNewSkillSourceDotagentsMissingOnDisk: dotagents configured but not on disk still resolves
// format-markdown/plan-work from the embedded copy; buildFromConfig hard-fails startup without them.
func TestNewSkillSourceDotagentsMissingOnDisk(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	ctx := context.Background()
	for _, name := range []string{"quack:format-markdown", "quack:plan-work"} {
		if _, err := src.LoadFrontmatter(ctx, name); err != nil {
			t.Errorf("LoadFrontmatter(%q) via embedded dotagents fallback: %v", name, err)
		}
	}
}

// TestNewSkillSourceDotagentsOnDiskNoDuplicate: a plugin providing bare format-markdown suppresses the
// embedded quack:format-markdown backfill (the dotagents half shadows by bare name against any plugin).
func TestNewSkillSourceDotagentsOnDiskNoDuplicate(t *testing.T) {
	dotagents := t.TempDir()
	writePluginManifest(t, dotagents, "dotagents")
	writeVendorSkill(t, filepath.Join(dotagents, "skills"), "format-markdown", "fixture")

	src := newSkillSource(resolvePlugins([]string{dotagents}))
	ctx := context.Background()
	if _, err := src.ListFrontmatters(ctx); err != nil {
		t.Fatalf("ListFrontmatters: %v (embedded fallback likely double-added dotagents)", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "dotagents:format-markdown"); err != nil {
		t.Errorf("LoadFrontmatter(dotagents:format-markdown): %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "quack:format-markdown"); err == nil {
		t.Error("LoadFrontmatter(quack:format-markdown) succeeded, want not-found - dotagents on disk must suppress the embedded copy")
	}
}

// TestAcpSkillPathsResolvesDotagents: dotagents' skills dir reaches an ACP agent's skills.paths through
// ordinary plugin discovery (it ships a root plugin.json).
func TestAcpSkillPathsResolvesDotagents(t *testing.T) {
	dotagents := t.TempDir()
	writePluginManifest(t, dotagents, "dotagents")
	writeVendorSkill(t, filepath.Join(dotagents, "skills"), "review-code", "fixture")

	ponytail := t.TempDir()
	writePluginManifest(t, ponytail, "ponytail")
	writeVendorSkill(t, filepath.Join(ponytail, "skills"), "ponytail", "fixture")

	paths := acpSkillPaths(resolvePlugins([]string{dotagents, ponytail}))

	wantDotagents := filepath.Join(dotagents, "skills")
	if !slices.Contains(paths, wantDotagents) {
		t.Fatalf("acpSkillPaths() = %v, want dotagents skills dir %q", paths, wantDotagents)
	}
	if _, err := os.Stat(filepath.Join(wantDotagents, "review-code", "SKILL.md")); err != nil {
		t.Errorf("review-code not found under the resolved dotagents dir: %v", err)
	}

	wantPonytail := filepath.Join(ponytail, "skills")
	if !slices.Contains(paths, wantPonytail) {
		t.Errorf("acpSkillPaths() = %v, want ponytail's resolved skills dir %q", paths, wantPonytail)
	}
}

// TestWorkflowCatalogNoShapesIsByteIdentical: with no custom shapes, the real plan-work skill through
// newSkillSource is byte-identical to the shipped instructions.
func TestWorkflowCatalogNoShapesIsByteIdentical(t *testing.T) {
	src := newSkillSource(nil)
	want, err := src.LoadInstructions(context.Background(), "quack:plan-work")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := workflowcatalog.WrapRef(src, shapesRefOf(workflowcatalog.FromConfig(nil, "rev")))
	got, err := wrapped.LoadInstructions(context.Background(), "quack:plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("plan-work instructions changed with zero configured workflow shapes")
	}
}

// TestWorkflowCatalogComposesIntoRealSkill: a configured shape lands in the table
// load_skill("quack:plan-work") returns to the orchestrator.
func TestWorkflowCatalogComposesIntoRealSkill(t *testing.T) {
	src := newSkillSource(nil)
	shapes := workflowcatalog.FromConfig([]config.WorkflowShape{{
		Name: "document-ingest", Trigger: "Ingest a new document into the knowledge base",
		Agents: []string{"document-classifier"},
		Shape:  "ONE `document-classifier` node (terminal - files the document in the KB)",
	}}, "rev")
	got, err := workflowcatalog.WrapRef(src, shapesRefOf(shapes)).LoadInstructions(context.Background(), "quack:plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| Ingest a new document into the knowledge base | ONE `document-classifier` node (terminal - files the document in the KB) |") {
		t.Errorf("composed plan-work instructions missing the custom shape's row:\n%s", got)
	}
}

// TestInitSkillsShippedSeedResolvesHardRequiredSkills: with the shipped seed (dotagents on disk shadowing
// the embedded copy), assembleOrchestrator's two hard-required bare-name lookups still resolve.
func TestInitSkillsShippedSeedResolvesHardRequiredSkills(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)

	dotagents := t.TempDir()
	writePluginManifest(t, dotagents, "dotagents")
	writeVendorSkill(t, filepath.Join(dotagents, "skills"), "format-markdown", "fixture")

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{
		Plugins: &config.PluginsConfig{
			Root: t.TempDir(),
			Seed: []string{dotagents, ".agents/plugins/usage"},
		},
	}}
	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}
	if fms, err := skills.builtinSkillSrc.ListFrontmatters(context.Background()); err != nil || len(fms) == 0 {
		t.Fatalf("ListFrontmatters: %v (%d skills)", err, len(fms))
	}

	for _, name := range []string{"format-markdown", "plan-work"} {
		if _, err := skillsource.Resolve(context.Background(), skills.skillSrc, name); err != nil {
			t.Errorf("skillsource.Resolve(%q) against the shipped seed: %v", name, err)
		}
	}
}

// TestShippedSeedRosterAndAcpPathsMatchPrePluginRegistryCounts: the shipped seed serves one copy of each skill
// (28) and 3 ACP skill paths; local fixtures stand in for registry-fetched plugins (no network in tests).
func TestShippedSeedRosterAndAcpPathsMatchPrePluginRegistryCounts(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)

	daEntries, err := os.ReadDir(dotagentsEmbeddedSkills)
	if err != nil {
		t.Fatal(err)
	}
	daRoot := t.TempDir()
	writePluginManifest(t, daRoot, "dotagents")
	for _, e := range daEntries {
		if !e.IsDir() {
			continue
		}
		writeVendorSkill(t, filepath.Join(daRoot, "skills"), e.Name(), "fixture")
	}

	ptRoot := t.TempDir()
	writePluginManifest(t, ptRoot, "ponytail")
	// ponytail isn't tracked anywhere in-repo, so these names are a fixed stand-in, not read from a real tree.
	for _, name := range []string{"ponytail", "ponytail-audit", "ponytail-debt", "ponytail-gain", "ponytail-help", "ponytail-review"} {
		writeVendorSkill(t, filepath.Join(ptRoot, "skills"), name, "fixture")
	}

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{
		Plugins: &config.PluginsConfig{
			Root: t.TempDir(),
			Seed: []string{daRoot, ptRoot, ".agents/plugins/usage"},
		},
	}}
	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fms, err := skills.builtinSkillSrc.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fms) != 28 {
		names := make([]string, len(fms))
		for i, fm := range fms {
			names[i] = fm.Name
		}
		t.Errorf("roster = %d skills, want 28 (one copy of each shipped skill): %v", len(fms), names)
	}

	paths := acpSkillPaths(skills.plugins)
	if len(paths) != 3 {
		t.Errorf("acpSkillPaths = %v (%d), want 3 (main's count)", paths, len(paths))
	}
}

// TestPluginSkillsLoadWithResources: a module-gated plugin's skills and resources load through
// the same merged source agents read.
func TestPluginSkillsLoadWithResources(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{sleeperPluginRoot}))
	ctx := context.Background()

	cases := map[string]string{"sleeper:fixture-skill": "references/report.md"}
	for name, resource := range cases {
		fm, err := src.LoadFrontmatter(ctx, name)
		if err != nil {
			t.Fatalf("LoadFrontmatter(%q): %v", name, err)
		}
		if fm.Name != name {
			t.Errorf("frontmatter name = %q, want %q", fm.Name, name)
		}
		if _, err := src.LoadInstructions(ctx, name); err != nil {
			t.Errorf("LoadInstructions(%q): %v", name, err)
		}
		rc, err := src.LoadResource(ctx, name, resource)
		if err != nil {
			t.Errorf("LoadResource(%q, %q): %v", name, resource, err)
			continue
		}
		rc.Close()
	}
}
