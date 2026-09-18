package serve

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestSkillsLoad guards against a skill in THIS repo whose SKILL.md frontmatter
// fails the skilltoolset's validation (bad name, description over the 1024-char
// ceiling, …). Both libraries are checked: the shipped skills/ (a bad one crashes startup) and .claude/skills/ (quack's own project skills - a bad one poisons every agent that clones quack, exactly as `huh-wizard`/`go-testing` did at 1045 and 1039 description chars).
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

// writeVendorSkill lays down one SKILL.md under dir/<name>/ in the exact
// layout the vendored ponytail skills/ dir ships (and the shipped skills/
// library uses).
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

// TestNewSkillSourceMergesVendoredSkills proves the plugin wiring: a root shaped
// like a real plugin (root plugin.json + skills/, two SKILL.md skills modeled here)
// resolves through newSkillSource alongside the shipped skills, merged into one Source. This is the contract the code-implementer's prompt depends on (load_skill("ponytail") / load_skill("ponytail-review")) once its plugin root is configured and initialised.
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
	// The primary (shipped) library still resolves through the same merged
	// source, as the embedded quack plugin (#1427 S2). NOTE: bundledir falls
	// back to the embedded copy here (cwd is this package dir).
	if _, err := src.LoadFrontmatter(ctx, "quack:plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(quack:plan-work) via merged source: %v", err)
	}
}

// TestNewSkillSourceMissingPluginRoot proves the Forbidden-section contract:
// a configured plugin root that doesn't exist on disk (an operator pointing
// skills.plugins at a path they never created) never fails the run - it's just absent from the merged source, and quack's own shipped skills resolve.
func TestNewSkillSourceMissingPluginRoot(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	if _, err := src.LoadFrontmatter(context.Background(), "quack:plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(quack:plan-work): %v", err)
	}
}

// TestNewSkillSourceNoPluginsConfigured proves the zero-plugins case: quack's
// own shipped skills resolve, and so does format-markdown - dotagents' go:embed'd
// copy (dotagentsEmbeddedSkills) fills in whenever plugin discovery didn't find it on disk, regardless of what's configured. ponytail has no such fallback and stays absent.
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

// TestNewSkillSourceDotagentsMissingOnDisk pins the regression a reviewer caught:
// dotagents configured as a plugin root but not checked out on disk (a standalone
// install outside any repo checkout, where /.agents was not mounted) must still resolve format-markdown/plan-work - buildFromConfig hard-fails startup without them, and before dotagentsEmbeddedSkills existed, losing disk access to dotagents meant losing the server entirely, not just a skill.
func TestNewSkillSourceDotagentsMissingOnDisk(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	ctx := context.Background()
	for _, name := range []string{"quack:format-markdown", "quack:plan-work"} {
		if _, err := src.LoadFrontmatter(ctx, name); err != nil {
			t.Errorf("LoadFrontmatter(%q) via embedded dotagents fallback: %v", name, err)
		}
	}
}

// TestNewSkillSourceDotagentsOnDiskNoDuplicate proves the embedded fallback
// is suppressed once dotagents already resolved via plugin discovery - a
// plugin registered as "dotagents" providing bare format-markdown suppresses
// the embedded quack:format-markdown backfill (#1427 R1: the vendored-
// dotagents half of the embedded bundle shadows by BARE name against any
// resolved plugin, not only a row literally named "quack" - that rule
// covers only quack's own skills/ half, e.g. plan-work).
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

// TestAcpSkillPathsResolvesDotagents proves dotagents' skills dir reaches an
// ACP agent's skills.paths through ordinary plugin discovery - dotagents now
// ships a root plugin.json (no backfill needed; see git history for the #864 workaround this replaced once the manifest landed upstream).
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

// TestWorkflowCatalogNoShapesIsByteIdentical is issue #805 test case 2, at the
// full wiring level (real shipped skills/plan-work/SKILL.md through
// newSkillSource): a deployment with no custom shapes must get the exact same plan-work instructions as before the extension point existed.
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

// TestWorkflowCatalogComposesIntoRealSkill is issue #805 test case 1 against
// the real shipped skill: a configured shape lands in the same table
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

// TestInitSkillsShippedSeedResolvesHardRequiredSkills is #1427 S1: boots
// initSkills with the real shipped default seed (dotagents on disk shadows
// the embedded quack:format-markdown), and proves the two hard-required
// bare-name lookups assembleOrchestrator does still resolve - they used to
// hard-fail boot once dotagents' skills became "dotagents:format-markdown".
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

// TestShippedSeedRosterAndAcpPathsMatchPrePluginRegistryCounts is the
// reviewer-mandated regression for #1427 R1: the shipped default seed on a
// dev checkout must serve the SAME 27-skill roster and 3 ACP skill paths as
// main did before the plugin registry existed - not a doubled roster from
// missing by-bare-name suppression of the embedded dotagents copy. Local
// fixtures stand in for the real registry-fetched dotagents/ponytail (no
// test may hit the network); the dotagents fixture's bare names are read
// from the tracked embedded snapshot so suppression fires exactly as it
// would against a real fetch.
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
	if len(fms) != 27 {
		names := make([]string, len(fms))
		for i, fm := range fms {
			names[i] = fm.Name
		}
		t.Errorf("roster = %d skills, want 27 (main's count): %v", len(fms), names)
	}

	paths := acpSkillPaths(skills.plugins)
	if len(paths) != 3 {
		t.Errorf("acpSkillPaths = %v (%d), want 3 (main's count)", paths, len(paths))
	}
}

// TestShippedSleeperPluginSkillsLoad mirrors TestNewSkillSourceMergesVendoredSkills
// against the real shipped .agents/plugins/sleeper root (skill-only: no
// extensions block, so admission never checks it against a linked module) -
// each of its three skills and one representative resource per skill must
// load through the same merged source an ACP/native agent actually reads.
func TestShippedSleeperPluginSkillsLoad(t *testing.T) {
	src := newSkillSource(resolvePlugins([]string{"../../.agents/plugins/sleeper"}))
	ctx := context.Background()

	cases := map[string]string{
		"sleeper:start-sit":       "references/report.md",
		"sleeper:waivers":         "references/faab-bidding.md",
		"sleeper:injury-and-news": "references/sources.md",
	}
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
