package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
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
// layout the ponytail submodule's skills/ dir ships (and the shipped skills/
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

	src := newSkillSource([]string{vendor})
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
// a configured plugin root that doesn't exist on disk (submodule not
// initialised) never fails the run - it's just absent from the merged
// source, and quack's own shipped skills still resolve.
func TestNewSkillSourceMissingPluginRoot(t *testing.T) {
	src := newSkillSource([]string{filepath.Join(t.TempDir(), "does-not-exist")})
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
// (a standalone install outside any repo checkout, or a worktree that
// skipped `git submodule update --init`) must still resolve
// format-markdown/plan-work - buildFromConfig hard-fails startup without
// them, and before dotagentsEmbeddedSkills existed, losing disk access to
// dotagents meant losing the server entirely, not just a skill.
func TestNewSkillSourceDotagentsMissingOnDisk(t *testing.T) {
	src := newSkillSource([]string{filepath.Join(t.TempDir(), "does-not-exist")})
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
		t.Skip("vendored dotagents submodule not initialised (git submodule update --init)")
	}
	src := newSkillSource([]string{dotagents})
	if _, err := src.ListFrontmatters(context.Background()); err != nil {
		t.Fatalf("ListFrontmatters: %v (embedded fallback likely double-added dotagents)", err)
	}
}
