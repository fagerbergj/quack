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
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

// TestSkillsLoad guards against a skill in THIS repo whose SKILL.md frontmatter
// fails the skilltoolset's validation (bad name, description over the 1024-char
// ceiling, …). Both libraries are checked: the shipped skills/ (a bad one crashes
// startup) and .claude/skills/ (quack's own project skills - a bad one poisons
// every agent that clones quack, exactly as `huh-wizard`/`go-testing` did at 1045
// and 1039 description chars).
func TestSkillsLoad(t *testing.T) {
	for _, dir := range []string{"../../skills", "../../.claude/skills", "../../.agents/vendor/dotagents/skills"} {
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

// TestNewSkillSourceMergesVendoredSkills proves the plugin wiring: a root
// shaped like a real plugin (root plugin.json + skills/, two SKILL.md skills
// modeled here) resolves through newSkillSource alongside the shipped
// skills, merged into one Source. This is the contract the code-implementer's
// prompt depends on (load_skill("ponytail") / load_skill("ponytail-review"))
// once its plugin root is configured and initialised.
func TestNewSkillSourceMergesVendoredSkills(t *testing.T) {
	vendor := t.TempDir()
	writePluginManifest(t, vendor, "ponytail")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "ponytail", "Forces the laziest solution that actually works.")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "ponytail-review", "Code review focused exclusively on over-engineering.")

	src := newSkillSource(plugin.ResolveSkillDirs([]string{vendor}))
	ctx := context.Background()

	for _, name := range []string{"ponytail", "ponytail-review"} {
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
	// source. NOTE: bundledir falls back to the embedded copy here (cwd is
	// this package dir, so no skills/ on disk) - proving the installed-binary
	// path too.
	if _, err := src.LoadFrontmatter(ctx, "plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(plan-work) via merged source: %v", err)
	}
}

// TestNewSkillSourceMissingPluginRoot proves the Forbidden-section contract:
// a configured plugin root that doesn't exist on disk (an operator pointing
// skills.plugins at a path they never created) never fails the run - it's
// just absent from the merged source, and quack's own shipped skills resolve.
func TestNewSkillSourceMissingPluginRoot(t *testing.T) {
	src := newSkillSource(plugin.ResolveSkillDirs([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	if _, err := src.LoadFrontmatter(context.Background(), "plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(plan-work): %v", err)
	}
}

// TestNewSkillSourceNoPluginsConfigured proves the zero-plugins case: quack's
// own shipped skills resolve, and so does format-markdown - dotagents'
// go:embed'd copy (dotagentsEmbeddedSkills) fills in whenever plugin
// discovery didn't find it on disk, regardless of what's configured. ponytail
// has no such fallback and stays absent.
func TestNewSkillSourceNoPluginsConfigured(t *testing.T) {
	src := newSkillSource(nil)
	ctx := context.Background()
	if _, err := src.LoadFrontmatter(ctx, "plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(plan-work) via quack's own shipped skills: %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "format-markdown"); err != nil {
		t.Errorf("LoadFrontmatter(format-markdown) via embedded dotagents fallback: %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "ponytail"); err == nil {
		t.Error("LoadFrontmatter(ponytail): want not-found without any plugin roots configured")
	}
}

// TestNewSkillSourceDotagentsMissingOnDisk pins the regression a reviewer
// caught: dotagents configured as a plugin root but not checked out on disk
// (a standalone install outside any repo checkout, where /.agents was not
// mounted) must still resolve
// format-markdown/plan-work - buildFromConfig hard-fails startup without
// them, and before dotagentsEmbeddedSkills existed, losing disk access to
// dotagents meant losing the server entirely, not just a skill.
func TestNewSkillSourceDotagentsMissingOnDisk(t *testing.T) {
	src := newSkillSource(plugin.ResolveSkillDirs([]string{filepath.Join(t.TempDir(), "does-not-exist")}))
	ctx := context.Background()
	for _, name := range []string{"format-markdown", "plan-work"} {
		if _, err := src.LoadFrontmatter(ctx, name); err != nil {
			t.Errorf("LoadFrontmatter(%q) via embedded dotagents fallback: %v", name, err)
		}
	}
}

// TestNewSkillSourceDotagentsOnDiskNoDuplicate proves the embedded fallback
// is suppressed once dotagents already resolved via plugin discovery -
// MergedSource.ListFrontmatters errors on a skill name defined by two
// sources at once (ErrDuplicateSkill), so double-adding it here would break
// every normal startup instead of only protecting the missing-disk case.
func TestNewSkillSourceDotagentsOnDiskNoDuplicate(t *testing.T) {
	dotagents := "../../.agents/vendor/dotagents"
	if st, err := os.Stat(dotagents + "/skills"); err != nil || !st.IsDir() {
		t.Fatalf("vendored dotagents skills missing at %s/skills", dotagents)
	}
	src := newSkillSource(plugin.ResolveSkillDirs([]string{dotagents}))
	if _, err := src.ListFrontmatters(context.Background()); err != nil {
		t.Fatalf("ListFrontmatters: %v (embedded fallback likely double-added dotagents)", err)
	}
}

// TestAcpSkillPathsResolvesDotagents proves dotagents' skills dir reaches an
// ACP agent's skills.paths through ordinary plugin discovery - dotagents now
// ships a root plugin.json (no backfill needed; see git history for the
// #864 workaround this replaced once the manifest landed upstream).
func TestAcpSkillPathsResolvesDotagents(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)

	paths := acpSkillPaths(plugin.ResolveSkillDirs([]string{".agents/vendor/dotagents", ".agents/vendor/ponytail"}))

	wantDotagents := filepath.Join(root, ".agents", "vendor", "dotagents", "skills")
	if !slices.Contains(paths, wantDotagents) {
		t.Fatalf("acpSkillPaths() = %v, want dotagents skills dir %q", paths, wantDotagents)
	}
	if _, err := os.Stat(filepath.Join(wantDotagents, "review-code", "SKILL.md")); err != nil {
		t.Errorf("review-code not found under the resolved dotagents dir: %v", err)
	}

	wantPonytail := filepath.Join(root, ".agents", "vendor", "ponytail", "skills")
	if !slices.Contains(paths, wantPonytail) {
		t.Errorf("acpSkillPaths() = %v, want ponytail's resolved skills dir %q", paths, wantPonytail)
	}
}

// TestWorkflowCatalogNoShapesIsByteIdentical is issue #805 test case 2, at
// the full wiring level (real shipped skills/plan-work/SKILL.md through
// newSkillSource): a deployment with no custom shapes must get the exact
// same plan-work instructions as before the extension point existed.
func TestWorkflowCatalogNoShapesIsByteIdentical(t *testing.T) {
	src := newSkillSource(nil)
	want, err := src.LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := workflowcatalog.Wrap(src, workflowcatalog.FromConfig(nil, "rev"))
	got, err := wrapped.LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("plan-work instructions changed with zero configured workflow shapes")
	}
}

// TestWorkflowCatalogComposesIntoRealSkill is issue #805 test case 1 against
// the real shipped skill: a configured shape lands in the same table
// load_skill("plan-work") returns to the orchestrator.
func TestWorkflowCatalogComposesIntoRealSkill(t *testing.T) {
	src := newSkillSource(nil)
	shapes := workflowcatalog.FromConfig([]config.WorkflowShape{{
		Name: "document-ingest", Trigger: "Ingest a new document into the knowledge base",
		Agents: []string{"document-classifier"},
		Shape:  "ONE `document-classifier` node (terminal - files the document in the KB)",
	}}, "rev")
	got, err := workflowcatalog.Wrap(src, shapes).LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| Ingest a new document into the knowledge base | ONE `document-classifier` node (terminal - files the document in the KB) |") {
		t.Errorf("composed plan-work instructions missing the custom shape's row:\n%s", got)
	}
}
