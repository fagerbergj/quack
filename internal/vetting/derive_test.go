package vetting

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// Regression (live e2e 2026-07-12): the planner cannot know a repo's check
// commands — it authors the DAG before anything has looked at the repo — so PR
// #180's "checks are mandatory" backstop forced it to GUESS (`go build` for a
// JavaScript repo) and rejected 7 plans in a row; zero nodes ever ran. Checks
// are a property of the REPO and are derived from it here, at gate time.

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDeriveChecksFromPackageJSON(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"package.json": `{"scripts": {"build": "next build", "lint": "next lint", "test": "vitest run", "dev": "next dev"}}`,
	})
	got := deriveChecks(dir, []string{"npm run"})
	want := []string{"npm run build", "npm run lint", "npm run test"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deriveChecks = %v, want %v (the repo's OWN scripts only — no invented `npx tsc`, no `dev`)", got, want)
	}
}

func TestDeriveChecksPackageJSONOnlyDeclaredScripts(t *testing.T) {
	dir := writeRepo(t, map[string]string{"package.json": `{"scripts": {"build": "next build"}}`})
	got := deriveChecks(dir, []string{"npm run"})
	if want := []string{"npm run build"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deriveChecks = %v, want %v", got, want)
	}
}

func TestDeriveChecksFromGoMod(t *testing.T) {
	dir := writeRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.24\n"})
	got := deriveChecks(dir, []string{"go build", "go vet", "go test"})
	want := []string{"go build ./...", "go vet ./...", "go test ./..."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deriveChecks = %v, want %v", got, want)
	}
}

func TestDeriveChecksFromMakefile(t *testing.T) {
	dir := writeRepo(t, map[string]string{"Makefile": "SHELL := /bin/sh\n\nbuild:\n\techo hi\n\ntest: build\n\techo t\n"})
	got := deriveChecks(dir, []string{"make"})
	want := []string{"make build", "make test"} // no `lint` target ⇒ no `make lint`
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deriveChecks = %v, want %v", got, want)
	}
}

func TestDeriveChecksDropsCommandsOutsideAllowlist(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"package.json": `{"scripts": {"build": "next build", "lint": "next lint"}}`,
	})
	got := deriveChecks(dir, []string{"npm run build"}) // only build is allowed
	if want := []string{"npm run build"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deriveChecks = %v, want %v (a command outside the allowlist must be dropped)", got, want)
	}
}

func TestDeriveChecksUnknownRepoYieldsNone(t *testing.T) {
	dir := writeRepo(t, map[string]string{"README.md": "# hi"})
	if got := deriveChecks(dir, []string{"npm run", "go build"}); len(got) != 0 {
		t.Errorf("deriveChecks = %v, want none (an unrecognised repo must NOT fail the node)", got)
	}
}

func TestDeriveChecksEmptyAllowlistYieldsNone(t *testing.T) {
	dir := writeRepo(t, map[string]string{"go.mod": "module example.com/x\n"})
	if got := deriveChecks(dir, nil); len(got) != 0 {
		t.Errorf("deriveChecks = %v, want none (no allowlist ⇒ checks stay disabled)", got)
	}
}

// End-to-end through the criterion: a code-implementer node with NO planner-set
// checks and NO workdir still gets checks — the gate finds the one repo in its
// workspace scope and derives them from it.
func TestChecksPassCriterionDerivesWhenPlannerSetNone(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := j.Resolve("u1", "c1", "")
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A repo whose only check script exits non-zero: the derived check must run
	// and fail the node (proving derivation happened and executed in the repo).
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("build:\n\t@exit 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DeriveChecks: true, CheckCommands: []string{"make"},
		Workspace: j, WorkspaceUserID: "u1", ChatID: "c1", WorkspaceCaps: workspace.DefaultCaps(),
		NodeID: "impl",
	}
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply — the repo declares a `build` target")
	}
	if got.Score != 0 || !strings.Contains(got.Reason, "make build") {
		t.Errorf("got %+v, want Score 0 naming the derived `make build` check", got)
	}
}

// No repo in the workspace ⇒ the criterion simply doesn't apply. A node must
// never FAIL because the planner omitted `workdir`/`checks`.
func TestChecksPassCriterionNoRepoSkipsRatherThanFails(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Resolve("u1", "c1", ""); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DeriveChecks: true, CheckCommands: []string{"go build"},
		Workspace: j, WorkspaceUserID: "u1", ChatID: "c1", WorkspaceCaps: workspace.DefaultCaps(),
	}
	if _, ok := checksPassCriterion(context.Background(), cfg); ok {
		t.Error("checks_pass must not apply when there's no repo to derive checks from")
	}
}

// mkRepo makes dir a git repo (a .git dir) carrying the given files.
func mkRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// scopeCfg is a derive-checks Config over a fresh jail, with the node's workdir.
func scopeCfg(t *testing.T, workdir string, allow ...string) (Config, string) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := j.Resolve("u1", "c1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{
		DeriveChecks: true, CheckCommands: allow, Workdir: workdir,
		Workspace: j, WorkspaceUserID: "u1", ChatID: "c1", WorkspaceCaps: workspace.DefaultCaps(),
		NodeID: "impl",
	}, root
}

// Regression (live e2e 2026-07-13): the planner set no usable workdir, so the
// checks dir resolved to the workspace SCOPE ROOT — which holds no package.json.
// "no checks derived from the repo; skipping checks" ⇒ checks never ran ⇒ code
// that does not typecheck passed the gate at 0.7. The repo was one level down
// (<scope>/games), where git_clone put it: SEARCH for it.
func TestChecksDirFindsRepoBelowTheScopeRoot(t *testing.T) {
	for _, workdir := range []string{"", ".", "games"} {
		t.Run("workdir="+workdir, func(t *testing.T) {
			cfg, root := scopeCfg(t, workdir, "npm run")
			repo := mkRepo(t, filepath.Join(root, "games"), map[string]string{
				"package.json": `{"scripts": {"build": "next build", "test": "vitest run"}}`,
			})
			dir, ok, err := checksDir(cfg)
			if err != nil || !ok {
				t.Fatalf("checksDir = (%q, %v, %v), want the repo dir", dir, ok, err)
			}
			if dir != repo {
				t.Fatalf("checksDir = %q, want %q (the repo, not the scope root)", dir, repo)
			}
			want := []string{"npm run build", "npm run test"}
			if got := deriveChecks(dir, cfg.CheckCommands); !reflect.DeepEqual(got, want) {
				t.Errorf("deriveChecks = %v, want %v", got, want)
			}
		})
	}
}

// The scope root ITSELF being the repo keeps working.
func TestChecksDirScopeRootIsTheRepo(t *testing.T) {
	cfg, root := scopeCfg(t, "", "go build")
	mkRepo(t, root, map[string]string{"go.mod": "module example.com/x\n"})
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok || dir != root {
		t.Fatalf("checksDir = (%q, %v, %v), want the scope root %q", dir, ok, err, root)
	}
}

// Two repos ⇒ ambiguous ⇒ no checks (never guess which tree is "the" repo).
func TestChecksDirAmbiguousReposSkips(t *testing.T) {
	cfg, root := scopeCfg(t, "", "npm run")
	mkRepo(t, filepath.Join(root, "games"), map[string]string{"package.json": `{"scripts":{"build":"x"}}`})
	mkRepo(t, filepath.Join(root, "other"), map[string]string{"package.json": `{"scripts":{"build":"x"}}`})
	if _, ok, err := checksDir(cfg); ok || err != nil {
		t.Errorf("checksDir applied (%v, %v) with two repos in scope; want skip", ok, err)
	}
	if _, ok := checksPassCriterion(context.Background(), cfg); ok {
		t.Error("checks_pass must not apply when the repo is ambiguous")
	}
}

// No repo at all ⇒ skip, no error (a research node's workspace).
func TestChecksDirNoRepoSkips(t *testing.T) {
	cfg, root := scopeCfg(t, "", "npm run")
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := checksDir(cfg); ok || err != nil {
		t.Errorf("checksDir = (%v, %v), want skip with no error", ok, err)
	}
}

// A vendored .git under node_modules must not be mistaken for the repo (and must
// not make the real repo look "ambiguous").
func TestChecksDirIgnoresNodeModules(t *testing.T) {
	cfg, root := scopeCfg(t, "", "npm run")
	repo := mkRepo(t, filepath.Join(root, "games"), map[string]string{"package.json": `{"scripts":{"build":"x"}}`})
	mkRepo(t, filepath.Join(repo, "node_modules", "dep"), map[string]string{"package.json": `{"scripts":{"build":"y"}}`})
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok || dir != repo {
		t.Fatalf("checksDir = (%q, %v, %v), want %q", dir, ok, err, repo)
	}
}

// Bug 2 (same live run): a failing check's FULL output was folded into the
// revise prompt, which grew past the context window until compaction truncated
// the worker's own task prompt and the revision worker failed outright. Bound it.
func TestBoundCheckOutputTruncatesLargeOutput(t *testing.T) {
	huge := strings.Repeat("src/app/page.tsx(12,5): error TS2322: Type 'x' is not assignable to type 'y'.\n", 200)
	got := boundCheckOutput(huge)
	if len(got) > maxCheckOutputChars+400 {
		t.Errorf("boundCheckOutput kept %d chars, want ≲ %d", len(got), maxCheckOutputChars)
	}
	if !strings.Contains(got, "check output truncated") {
		t.Errorf("boundCheckOutput = %q…, want a truncation marker naming how much was dropped", got[:80])
	}
	if !strings.Contains(got, "error TS2322") {
		t.Error("boundCheckOutput dropped the actual (first) errors")
	}
}

func TestBoundCheckOutputPassesSmallOutputThrough(t *testing.T) {
	small := "FAIL src/game.test.ts\n  expected 1 to be 2\n"
	if got := boundCheckOutput(small); got != small {
		t.Errorf("boundCheckOutput = %q, want it unchanged", got)
	}
}

// The bounded output is what reaches the revise prompt (composeFeedback).
func TestComposeFeedbackCheckOutputIsBounded(t *testing.T) {
	huge := strings.Repeat("error TS2322: Type 'x' is not assignable.\n", 500)
	v := verdict{
		Feedback: "judge narrative",
		Criteria: map[string]criterionScore{
			"checks_pass": {Score: 0, Reason: "deterministic: check \"npm run build\" failed (exit 1):\n" + boundCheckOutput(huge)},
		},
	}
	if got := composeFeedback(v, 0.7); len(got) > maxCheckOutputChars+1_000 {
		t.Errorf("composeFeedback = %d chars, want the check output bounded to ~%d", len(got), maxCheckOutputChars)
	}
}
