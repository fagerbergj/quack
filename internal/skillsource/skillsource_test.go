package skillsource

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/workspace"
)

// writeSkill writes a minimal valid SKILL.md at dir/name/SKILL.md.
func writeSkill(t *testing.T, dir, name, desc, body string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup returns a jail and its "u1" user root, with a built-in source over a
// separate temp dir holding the given built-in skills.
func setup(t *testing.T, builtinSkills map[string]string) (*workspace.Jail, string, skill.Source) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userRoot, err := j.Resolve("u1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	builtinDir := t.TempDir()
	for name, body := range builtinSkills {
		writeSkill(t, builtinDir, name, "builtin "+name, body)
	}
	builtin := skill.NewFileSystemSource(os.DirFS(builtinDir))
	return j, userRoot, builtin
}

func names(fms []*skill.Frontmatter) map[string]bool {
	m := map[string]bool{}
	for _, fm := range fms {
		m[fm.Name] = true
	}
	return m
}

func TestProjectSkillIsLoadable(t *testing.T) {
	j, userRoot, builtin := setup(t, map[string]string{"plan-work": "builtin body"})
	// A cloned repo with a project skill under .agents/skills.
	writeSkill(t, filepath.Join(userRoot, "chatA", "myrepo", ".agents", "skills"), "repo-skill", "a project skill", "project body")

	src := New(builtin, j, "u1")

	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(fms); !got["plan-work"] || !got["repo-skill"] {
		t.Errorf("ListFrontmatters names = %v, want both plan-work and repo-skill", got)
	}
	ins, err := src.LoadInstructions(context.Background(), "repo-skill")
	if err != nil {
		t.Fatalf("LoadInstructions(repo-skill): %v", err)
	}
	if ins == "" {
		t.Error("project skill instructions were empty")
	}
}

func TestBuiltinWinsCollision(t *testing.T) {
	j, userRoot, builtin := setup(t, map[string]string{"plan-work": "BUILTIN WINS"})
	// A hostile repo tries to shadow the core plan-work skill.
	writeSkill(t, filepath.Join(userRoot, "chatA", "evil", ".agents", "skills"), "plan-work", "hijacked", "PROJECT LOSES")

	src := New(builtin, j, "u1")

	// LoadInstructions resolves to the built-in, never the project shadow.
	ins, err := src.LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ins, "BUILTIN WINS") || strings.Contains(ins, "PROJECT LOSES") {
		t.Errorf("LoadInstructions(plan-work) = %q, want the built-in body (not the project shadow)", ins)
	}
	// ListFrontmatters shows plan-work exactly once (the project duplicate hidden).
	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, fm := range fms {
		if fm.Name == "plan-work" {
			count++
			if fm.Description != "builtin plan-work" {
				t.Errorf("plan-work description = %q, want the built-in one", fm.Description)
			}
		}
	}
	if count != 1 {
		t.Errorf("plan-work listed %d times, want exactly 1 (project duplicate hidden)", count)
	}
}

func TestNoProjectDirsUnchanged(t *testing.T) {
	j, _, builtin := setup(t, map[string]string{"plan-work": "b", "ponytail": "p"})
	src := New(builtin, j, "u1")

	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(fms); !got["plan-work"] || !got["ponytail"] || len(got) != 2 {
		t.Errorf("names = %v, want exactly the two built-in skills", got)
	}
	// An unknown skill is still not found (project layer adds nothing).
	if _, err := src.LoadInstructions(context.Background(), "nope"); err == nil {
		t.Error("expected ErrSkillNotFound for an unknown skill")
	}
}

func TestClaudeSkillsDirAlsoDiscovered(t *testing.T) {
	j, userRoot, builtin := setup(t, nil)
	writeSkill(t, filepath.Join(userRoot, "chatA", "myrepo", ".claude", "skills"), "claude-skill", "from .claude", "body")

	fms := ProjectSkills(j, "u1", "chatA", "myrepo")
	if len(fms) != 1 || fms[0].Name != "claude-skill" {
		t.Errorf("ProjectSkills = %+v, want the .claude/skills one", fms)
	}
	_ = builtin
}

func TestNilJailReturnsBuiltin(t *testing.T) {
	builtinDir := t.TempDir()
	writeSkill(t, builtinDir, "x", "d", "b")
	builtin := skill.NewFileSystemSource(os.DirFS(builtinDir))
	if New(builtin, nil, "u1") != builtin {
		t.Error("New with a nil jail should return the built-in source unchanged")
	}
}

// writeBadSkill writes a SKILL.md whose frontmatter fails validation (a
// description over the skilltoolset's 1024-char ceiling) — the shape that took
// every project skill down in production.
func writeBadSkill(t *testing.T, dir, name string) {
	t.Helper()
	writeSkill(t, dir, name, strings.Repeat("x", 1100), "body")
}

// TestMalformedSkillDoesNotKillListing: one bad SKILL.md in a cloned repo must
// not disable the WHOLE project-skill listing (nor the built-in library). The
// bad skill is skipped; every skill that parsed is returned.
func TestMalformedSkillDoesNotKillListing(t *testing.T) {
	j, userRoot, builtin := setup(t, map[string]string{"plan-work": "builtin body"})
	skillsDir := filepath.Join(userRoot, "chatA", "myrepo", ".agents", "skills")
	writeSkill(t, skillsDir, "good-skill", "a valid project skill", "body")
	writeBadSkill(t, skillsDir, "bad-skill")

	src := New(builtin, j, "u1")

	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	got := names(fms)
	if !got["plan-work"] {
		t.Errorf("names = %v, want the built-in plan-work", got)
	}
	if !got["good-skill"] {
		t.Errorf("names = %v, want the valid project skill (a malformed sibling must not hide it)", got)
	}
	if got["bad-skill"] {
		t.Errorf("names = %v, want the malformed skill skipped", got)
	}
}

// TestMalformedSkillWarnsOnce: the same bad file must not re-warn on every skill
// call (the live run logged this hundreds of times in one run).
func TestMalformedSkillWarnsOnce(t *testing.T) {
	j, userRoot, builtin := setup(t, nil)
	skillsDir := filepath.Join(userRoot, "chatA", "myrepo", ".agents", "skills")
	writeBadSkill(t, skillsDir, "bad-skill")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	src := New(builtin, j, "u1")
	for range 3 {
		if _, err := src.ListFrontmatters(context.Background()); err != nil {
			t.Fatalf("ListFrontmatters: %v", err)
		}
	}

	if n := strings.Count(buf.String(), "bad-skill"); n != 1 {
		t.Errorf("bad skill warned %d times across 3 listings, want exactly 1:\n%s", n, buf.String())
	}
}

// TestScopedRestrictsListingAndLoad proves the per-agent scoping contract
// (#251): a name outside an agent's declared scope is invisible to
// ListFrontmatters and unloadable — exactly as if it never existed.
func TestScopedRestrictsListingAndLoad(t *testing.T) {
	_, _, builtin := setup(t, map[string]string{
		"develop-feature": "builtin body",
		"review-code":     "builtin body",
	})
	src := Scoped(builtin, []string{"develop-feature"})
	ctx := context.Background()

	fms, err := src.ListFrontmatters(ctx)
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	got := names(fms)
	if !got["develop-feature"] {
		t.Errorf("names = %v, want in-scope develop-feature listed", got)
	}
	if got["review-code"] {
		t.Errorf("names = %v, want out-of-scope review-code hidden", got)
	}

	if _, err := src.LoadFrontmatter(ctx, "develop-feature"); err != nil {
		t.Errorf("LoadFrontmatter(develop-feature): %v", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "review-code"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadFrontmatter(review-code) = %v, want ErrSkillNotFound (out of scope)", err)
	}
	if _, err := src.LoadInstructions(ctx, "review-code"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadInstructions(review-code) = %v, want ErrSkillNotFound (out of scope)", err)
	}
}

// TestScopedPreservesProjectSkillDiscovery proves a cloned repo's own project
// skills stay fully additive and unrestricted when Scoped is applied to only
// the built-in layer (the wiring internal/serve/serve.go uses): a web-researcher-
// shaped scope with NO code-review skills still sees a project skill discovered
// in the jail, and the built-in library still wins a name collision.
func TestScopedPreservesProjectSkillDiscovery(t *testing.T) {
	j, userRoot, builtin := setup(t, map[string]string{
		"plan-work":   "builtin body",
		"review-code": "builtin body",
	})
	skillsDir := filepath.Join(userRoot, "chatA", "myrepo", ".agents", "skills")
	writeSkill(t, skillsDir, "project-only-skill", "a project skill", "body")

	restricted := Scoped(builtin, []string{"plan-work"}) // this agent's built-in scope is just plan-work
	src := New(restricted, j, "u1")

	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	got := names(fms)
	if !got["plan-work"] {
		t.Errorf("names = %v, want the in-scope built-in plan-work listed", got)
	}
	if got["review-code"] {
		t.Errorf("names = %v, want out-of-scope built-in review-code hidden", got)
	}
	if !got["project-only-skill"] {
		t.Errorf("names = %v, want the project skill visible regardless of built-in scope", got)
	}

	// Collision precedence is unaffected by scoping: the built-in "plan-work"
	// still wins even though it's IN this agent's scope.
	writeSkill(t, skillsDir, "plan-work", "a hijack attempt", "body")
	fm, err := src.LoadFrontmatter(context.Background(), "plan-work")
	if err != nil {
		t.Fatalf("LoadFrontmatter(plan-work): %v", err)
	}
	if fm.Description != "builtin plan-work" {
		t.Errorf("LoadFrontmatter(plan-work).Description = %q, want the built-in one to win the collision", fm.Description)
	}
}
