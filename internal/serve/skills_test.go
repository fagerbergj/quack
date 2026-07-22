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
	for _, dir := range []string{"../../skills", "../../.claude/skills"} {
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

// TestNewSkillSourceMergesVendoredSkills proves the vendored-skills wiring:
// with a vendor dir shaped exactly like the ponytail submodule's skills/
// (five SKILL.md skills; two modeled here), newSkillSource resolves both the
// shipped skills (via bundledir's disk/embedded primary) AND the vendored
// ones through a single merged Source. This is the contract the
// code-implementer's prompt depends on (load_skill("ponytail") /
// load_skill("ponytail-review")) the moment `git submodule update --init`
// populates .agents/vendor/ponytail.
func TestNewSkillSourceMergesVendoredSkills(t *testing.T) {
	vendor := t.TempDir()
	writeVendorSkill(t, vendor, "ponytail", "Forces the laziest solution that actually works.")
	writeVendorSkill(t, vendor, "ponytail-review", "Code review focused exclusively on over-engineering.")

	src := newSkillSource(vendor)
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

// TestNewSkillSourceVendorAbsent proves the fallback: no vendor dir on disk
// (submodule not initialized, or a binary installed outside the repo) ⇒ the
// primary source alone, with no error and no ponytail skills.
func TestNewSkillSourceVendorAbsent(t *testing.T) {
	src := newSkillSource(filepath.Join(t.TempDir(), "does-not-exist"))
	ctx := context.Background()
	if _, err := src.LoadFrontmatter(ctx, "plan-work"); err != nil {
		t.Errorf("LoadFrontmatter(plan-work): %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "ponytail"); err == nil {
		t.Error("LoadFrontmatter(ponytail): want not-found without the vendor dir")
	}
}
