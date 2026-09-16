package skillsource

import (
	"context"
	"errors"
	"os"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

func TestPrefixedQualifiesNames(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "format-markdown", "desc", "body")
	src := Prefixed("dotagents", skill.NewFileSystemSource(os.DirFS(dir)))
	ctx := context.Background()

	fms, err := src.ListFrontmatters(ctx)
	if err != nil || len(fms) != 1 || fms[0].Name != "dotagents:format-markdown" {
		t.Fatalf("ListFrontmatters = %+v, err %v, want one dotagents:format-markdown", fms, err)
	}

	fm, err := src.LoadFrontmatter(ctx, "dotagents:format-markdown")
	if err != nil || fm.Name != "dotagents:format-markdown" {
		t.Fatalf("LoadFrontmatter(dotagents:format-markdown) = %+v, err %v", fm, err)
	}
	if _, err := src.LoadInstructions(ctx, "dotagents:format-markdown"); err != nil {
		t.Fatalf("LoadInstructions: %v", err)
	}

	// The bare (unqualified) name is not this source's business - Scoped's
	// bare-name fallback lives one layer up.
	if _, err := src.LoadFrontmatter(ctx, "format-markdown"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Fatalf("LoadFrontmatter(format-markdown) = %v, want ErrSkillNotFound", err)
	}
}

// TestMergedSourceAllowsSameSkillNameAcrossPlugins is the whole point of
// prefixing (#1427): two plugins shipping a same-named skill must not
// collide in skill.NewMergedSource, which errors on a literal duplicate name.
func TestMergedSourceAllowsSameSkillNameAcrossPlugins(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, dirA, "review-code", "a", "body a")
	writeSkill(t, dirB, "review-code", "b", "body b")

	merged := skill.NewMergedSource(
		Prefixed("alpha", skill.NewFileSystemSource(os.DirFS(dirA))),
		Prefixed("beta", skill.NewFileSystemSource(os.DirFS(dirB))),
	)
	fms, err := merged.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	if len(fms) != 2 {
		t.Fatalf("got %d frontmatters, want 2 (alpha:review-code, beta:review-code)", len(fms))
	}
}

func TestScopedBareNameMatchesQualified(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "format-markdown", "desc", "body")
	src := Scoped(Prefixed("dotagents", skill.NewFileSystemSource(os.DirFS(dir))), []string{"format-markdown"})
	ctx := context.Background()

	fms, err := src.ListFrontmatters(ctx)
	if err != nil || len(fms) != 1 || fms[0].Name != "dotagents:format-markdown" {
		t.Fatalf("ListFrontmatters = %+v, err %v, want the qualified name", fms, err)
	}
	if _, err := src.LoadFrontmatter(ctx, "dotagents:format-markdown"); err != nil {
		t.Fatalf("LoadFrontmatter(dotagents:format-markdown): %v", err)
	}
}

// TestScopedBareNameFirstPluginWins: a bare config name matching two
// plugins' skills resolves to only the FIRST source in merge order.
func TestScopedBareNameFirstPluginWins(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, dirA, "review-code", "a", "body a")
	writeSkill(t, dirB, "review-code", "b", "body b")
	merged := skill.NewMergedSource(
		Prefixed("alpha", skill.NewFileSystemSource(os.DirFS(dirA))),
		Prefixed("beta", skill.NewFileSystemSource(os.DirFS(dirB))),
	)
	src := Scoped(merged, []string{"review-code"})

	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	if len(fms) != 1 || fms[0].Name != "alpha:review-code" {
		t.Fatalf("ListFrontmatters = %+v, want exactly [alpha:review-code] (first plugin wins)", fms)
	}
}

// TestScopedRejectsExplicitlyQualifiedNameOutsideScope is #1427 R2: a scope
// of bare "review-code" resolves to alpha's copy (first plugin wins) - a
// caller that explicitly asks for beta's copy by its qualified name must get
// ErrSkillNotFound, never silently redirected to alpha's content instead.
func TestScopedRejectsExplicitlyQualifiedNameOutsideScope(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, dirA, "review-code", "a", "alpha body")
	writeSkill(t, dirB, "review-code", "b", "beta body")
	merged := skill.NewMergedSource(
		Prefixed("alpha", skill.NewFileSystemSource(os.DirFS(dirA))),
		Prefixed("beta", skill.NewFileSystemSource(os.DirFS(dirB))),
	)
	src := Scoped(merged, []string{"review-code"})
	ctx := context.Background()

	if _, err := src.LoadInstructions(ctx, "beta:review-code"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Fatalf("LoadInstructions(beta:review-code) = %v, want ErrSkillNotFound (out of scope, not silently redirected)", err)
	}
	if _, err := src.LoadFrontmatter(ctx, "beta:review-code"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Fatalf("LoadFrontmatter(beta:review-code) = %v, want ErrSkillNotFound", err)
	}
	// The scope's actual resolution (bare, or its qualified form) still works.
	if got, err := src.LoadInstructions(ctx, "alpha:review-code"); err != nil || got != "\nalpha body\n" {
		t.Fatalf("LoadInstructions(alpha:review-code) = %q, err %v, want alpha's body", got, err)
	}
}

// TestScopedBareNameLoadInstructionsForwardsResolvedName is #1427 F3: the old
// code forwarded the CALLER's bare name straight to the prefixed source,
// which doesn't understand it (not-found); and a scope of [review-code]
// must load alpha's copy for every method, never beta's.
func TestScopedBareNameLoadInstructionsForwardsResolvedName(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, dirA, "review-code", "a", "alpha body")
	writeSkill(t, dirB, "review-code", "b", "beta body")
	merged := skill.NewMergedSource(
		Prefixed("alpha", skill.NewFileSystemSource(os.DirFS(dirA))),
		Prefixed("beta", skill.NewFileSystemSource(os.DirFS(dirB))),
	)
	src := Scoped(merged, []string{"review-code"})
	ctx := context.Background()

	got, err := src.LoadInstructions(ctx, "review-code")
	if err != nil {
		t.Fatalf("LoadInstructions(review-code): %v", err)
	}
	if got != "\nalpha body\n" {
		t.Fatalf("LoadInstructions(review-code) = %q, want alpha's body (first plugin wins)", got)
	}
	if _, err := src.LoadFrontmatter(ctx, "review-code"); err != nil {
		t.Fatalf("LoadFrontmatter(review-code): %v", err)
	}
}

func TestResolve(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, dirA, "review-code", "a", "alpha body")
	writeSkill(t, dirB, "review-code", "b", "beta body")
	merged := skill.NewMergedSource(
		Prefixed("alpha", skill.NewFileSystemSource(os.DirFS(dirA))),
		Prefixed("beta", skill.NewFileSystemSource(os.DirFS(dirB))),
	)
	ctx := context.Background()

	fm, err := Resolve(ctx, merged, "review-code")
	if err != nil || fm.Name != "alpha:review-code" {
		t.Fatalf("Resolve(review-code) = %+v, err %v, want alpha:review-code", fm, err)
	}
	fm, err = Resolve(ctx, merged, "beta:review-code")
	if err != nil || fm.Name != "beta:review-code" {
		t.Fatalf("Resolve(beta:review-code) = %+v, err %v, want beta:review-code (qualified loads directly)", fm, err)
	}
	if _, err := Resolve(ctx, merged, "no-such-skill"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Fatalf("Resolve(no-such-skill) = %v, want ErrSkillNotFound", err)
	}
}
