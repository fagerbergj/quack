package vetting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func testChecksConfig(t *testing.T, checks []string, workdir string) Config {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := j.UserRoot("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{
		Checks: checks, Workdir: workdir,
		Workspace: j, WorkspaceUserID: "u1", WorkspaceCaps: workspace.DefaultCaps(),
	}
}

// TestChecksCapsUsesCheckTimeout proves the gate's check runner gets
// CheckTimeout, not WorkspaceCaps.Timeout (the run_command budget) - the
// root cause of #1171's `go test ./...` gate timeout at 60s.
func TestChecksCapsUsesCheckTimeout(t *testing.T) {
	cfg := testChecksConfig(t, nil, "")
	cfg.WorkspaceCaps.Timeout = 60 * time.Second
	cfg.CheckTimeout = 600 * time.Second
	if got := checksCaps(cfg).Timeout; got != 600*time.Second {
		t.Errorf("checksCaps Timeout = %v, want CheckTimeout (600s), not WorkspaceCaps.Timeout (60s)", got)
	}
	cfg.CheckTimeout = 0
	if got := checksCaps(cfg).Timeout; got != 60*time.Second {
		t.Errorf("checksCaps Timeout = %v, want WorkspaceCaps.Timeout (60s) when CheckTimeout unset", got)
	}
}

func TestChecksPassCriterionAllPass(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true", "true"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (all checks passed)", got.Score)
	}
}

func TestChecksPassCriterionOneFails(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true", "false"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0 (a check failed)", got.Score)
	}
	if !strings.Contains(got.Reason, "false") {
		t.Errorf("Reason = %q, want it to name the failing command", got.Reason)
	}
}

func TestChecksPassCriterionNoWorkspaceFailsClosed(t *testing.T) {
	cfg := Config{Checks: []string{"true"}} // Workspace deliberately nil
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply (explicit Checks, fail closed)")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 (fail closed with no workspace wired up)", got.Score)
	}
}

func TestChecksPassCriterionOutputInReason(t *testing.T) {
	// A real failing command with distinctive stderr output - ls on a path
	// that doesn't exist reliably prints a recognizable error and exits
	// non-zero, without needing a shell (RunArgv never invokes one).
	cfg := testChecksConfig(t, []string{"ls /quack-checks-test-does-not-exist-xyz"}, "")
	got, ok := checksPassCriterion(context.Background(), cfg)
	if !ok {
		t.Fatal("checks_pass should apply")
	}
	if got.Score != 0 {
		t.Fatalf("Score = %v, want 0", got.Score)
	}
	if !strings.Contains(got.Reason, "quack-checks-test-does-not-exist-xyz") {
		t.Errorf("Reason = %q, want the command's output tail", got.Reason)
	}
}

// TestChecksPassCriterionSkipReason_RecordsOnSpan guards the telemetry fix
// for quack's phantom-success history (a fabricated exploration once scored
// 0.9; a phantom delivery shipped): when checks_pass does not apply at all,
// "why" must land as a queryable span attribute, not just a slog.Info line
// invisible in Tempo. An empty Config (no Checks, DeriveChecks off) is the
// simplest of the four skip paths - see skipChecks in checks.go.
func TestChecksPassCriterionSkipReason_RecordsOnSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	_, ok := checksPassCriterionTraced(context.Background(), Config{})
	if ok {
		t.Fatal("checks_pass should not apply with nothing configured")
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	var gotReason string
	var gotApplicable bool
	for _, a := range spans[0].Attributes {
		switch string(a.Key) {
		case "skip_reason":
			gotReason = a.Value.AsString()
		case "applicable":
			gotApplicable = a.Value.AsBool()
		}
	}
	if gotApplicable {
		t.Error("applicable = true, want false - the criterion did not run")
	}
	if gotReason != skipReasonNotConfigured {
		t.Errorf("skip_reason = %q, want %q", gotReason, skipReasonNotConfigured)
	}
}

func TestFoldDeterministicFoldsChecksPass(t *testing.T) {
	cfg := testChecksConfig(t, []string{"false"}, "")
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 1}}}
	det, _ := computeDeterministicCriteria(context.Background(), "some answer", workerActivity{}, cfg)
	got := mergeDeterministic(v, det, cfg)
	c, ok := got.Criteria["checks_pass"]
	if !ok {
		t.Fatal("checks_pass criterion missing")
	}
	if c.Score != 0 {
		t.Errorf("checks_pass score = %v, want 0", c.Score)
	}
	if got.Score != 0 {
		t.Errorf("overall Score = %v, want 0 (weakest-link on checks_pass)", got.Score)
	}
}

func TestFoldDeterministicNodeWithoutChecksUntouched(t *testing.T) {
	cfg := Config{} // no Checks configured
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 1}}}
	det, _ := computeDeterministicCriteria(context.Background(), "some answer", workerActivity{}, cfg)
	got := mergeDeterministic(v, det, cfg)
	if _, ok := got.Criteria["checks_pass"]; ok {
		t.Fatal("checks_pass should not appear for a node with no Checks configured")
	}
}

func TestFoldDeterministicPassingChecksDoNotFail(t *testing.T) {
	cfg := testChecksConfig(t, []string{"true"}, "")
	v := verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 0.9}}}
	det, _ := computeDeterministicCriteria(context.Background(), "some answer", workerActivity{}, cfg)
	got := mergeDeterministic(v, det, cfg)
	c, ok := got.Criteria["checks_pass"]
	if !ok {
		t.Fatal("checks_pass criterion missing")
	}
	if c.Score != 1 {
		t.Errorf("checks_pass score = %v, want 1 (all checks passed)", c.Score)
	}
	if got.Score != 0.9 {
		t.Errorf("overall Score = %v, want 0.9 (checks_pass shouldn't drag it down)", got.Score)
	}
}

func TestComposeFeedbackAppendsFailingCriteriaReasons(t *testing.T) {
	v := verdict{
		Feedback: "judge's own narrative",
		Criteria: map[string]criterionScore{
			"answers_question": {Score: 0.9, Reason: "fine"},
			"checks_pass":      {Score: 0, Reason: "check \"go test ./...\" failed (exit 1): some failure"},
		},
	}
	_, got := composeFeedback(v, 0.7, 1)
	if !strings.Contains(got, "judge's own narrative") {
		t.Errorf("composeFeedback dropped the judge's own feedback: %q", got)
	}
	if !strings.Contains(got, "go test ./...") {
		t.Errorf("composeFeedback = %q, want the failing check's reason included", got)
	}
	if strings.Contains(got, "fine") {
		t.Errorf("composeFeedback = %q, should not include a PASSING criterion's reason", got)
	}
}

func TestComposeFeedbackNoFailuresReturnsJudgeFeedbackUnchanged(t *testing.T) {
	v := verdict{
		Feedback: "all good",
		Criteria: map[string]criterionScore{"answers_question": {Score: 1, Reason: "great"}},
	}
	_, got := composeFeedback(v, 0.7, 1)
	if got != "all good" {
		t.Errorf("composeFeedback = %q, want unchanged judge feedback %q", got, "all good")
	}
}

// TestChecksDirFindsTheNodesOwnRepo: two concurrent nodes each cloned a repo
// into the same chat scope. Derived checks must find THIS node's repo - a
// search from the chat root sees two repos, gives up ("no single repo"), and
// nothing gets gated.
func TestChecksDirFindsTheNodesOwnRepo(t *testing.T) {
	cfg := testChecksConfig(t, nil, "")
	cfg.ChatID = "chat-1"
	cfg.NodeID = "impl_node"
	cfg.DeriveChecks = true
	for _, rel := range []string{"impl_node/mine/.git", "other_node/theirs/.git"} {
		dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, rel)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir, ok, err := checksDir(cfg)
	if err != nil {
		t.Fatalf("checksDir: %v", err)
	}
	if !ok {
		t.Fatal("checksDir found no repo: a search from the chat root sees BOTH nodes' clones and gives up")
	}
	if !strings.HasSuffix(dir, "impl_node/mine") {
		t.Errorf("checksDir = %q, want the node's own clone (…/impl_node/mine)", dir)
	}
}

// writeTestRepo fakes a plain clone: a .git dir with a config naming an origin remote.
func writeTestRepo(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = " + originURL + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTestWorktree fakes a linked git worktree, the shape production actually
// uses for gate node dirs: dir/.git is a pointer FILE (not a directory) naming
// a gitdir under a separate parent clone, which holds the real config/origin.
func writeTestWorktree(t *testing.T, dir, originURL string) {
	t.Helper()
	parent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parent, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = " + originURL + "\n"
	if err := os.WriteFile(filepath.Join(parent, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// commondir is relative to the worktree's own gitdir, same as real git.
	if err := os.WriteFile(filepath.Join(parent, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := "gitdir: " + filepath.Join(parent, ".git", "worktrees", "wt") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(gitdir), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestChecksDirFallsBackToNodeDirForExplicitChecks pins the "fork/exec
// /quack" bug: the planner set explicit Checks and a Workdir equal to the
// repo name, but the clone was provisioned AT the node dir, not below a
// subdirectory named after the repo. checksDir must fall back to the node's
// own dir - but only because that dir IS positively identifiable as the repo
// "quack" named: it's a git repo/worktree whose origin ends in /quack.
// Worktree is the default case: production gate node dirs are linked
// worktrees (.git is a pointer file), not plain clones - see quack#1083.
func TestChecksDirFallsBackToNodeDirForExplicitChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir, originURL string)
	}{
		{"linked worktree", writeTestWorktree},
		{"plain clone", writeTestRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testChecksConfig(t, []string{"go build ./..."}, "quack")
			cfg.ChatID = "chat-1"
			cfg.NodeID = "review"
			nodeDir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, "review")
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, nodeDir, "git@github.com:fagerbergj/quack.git")
			if err := os.WriteFile(filepath.Join(nodeDir, "go.mod"), []byte("module quack\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok, err := checksDir(cfg)
			if err != nil {
				t.Fatalf("checksDir: %v", err)
			}
			if !ok {
				t.Fatal("checksDir found no dir: should fall back to the node's own dir")
			}
			if !strings.HasSuffix(got, "/review") {
				t.Errorf("checksDir = %q, want the node's own dir (…/review), not …/review/quack", got)
			}
		})
	}
}

// TestChecksDirRefusesFallbackToUnrelatedRepo guards the false-pass the
// reviewer found: a node dir already holds an unrelated buildable module (not
// the repo the planner's Workdir named), and Workdir names a subdirectory
// that was never created. checksDir must NOT fall back to the parent - that
// would silently build the wrong thing and report a false pass. Checked for
// a plain directory, a plain clone of an unrelated repo, and an unrelated
// repo checked out as a linked worktree.
func TestChecksDirRefusesFallbackToUnrelatedRepo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{"no .git at all", func(t *testing.T, dir string) {}},
		{"plain clone of a different repo", func(t *testing.T, dir string) {
			writeTestRepo(t, dir, "git@github.com:fagerbergj/other.git")
		}},
		{"linked worktree of a different repo", func(t *testing.T, dir string) {
			writeTestWorktree(t, dir, "git@github.com:fagerbergj/other.git")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testChecksConfig(t, []string{"go build ./..."}, "newservice")
			cfg.ChatID = "chat-1"
			cfg.NodeID = "impl"
			nodeDir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, "impl")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nodeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.write(t, nodeDir)
			if err := os.WriteFile(filepath.Join(nodeDir, "go.mod"), []byte("module unrelated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok, err := checksDir(cfg)
			if err != nil {
				t.Fatalf("checksDir: %v", err)
			}
			if ok && strings.HasSuffix(got, "/impl") {
				t.Fatalf("checksDir = %q, ok=%v: must not fall back onto the unrelated module at the node dir", got, ok)
			}
		})
	}
}

// TestChecksDirIgnoresGarbageWorkdirWhenDeriving guards #620: an ACP
// implement node with a pre-provisioned clone (plan.Setup) derives its
// checks (cfg.Checks empty, cfg.DeriveChecks true), where Workdir is
// documented as ignored (dag.Node.Workdir, vetting.Config.Workdir) - but the
// orchestrator model sometimes fills it in anyway ("/tmp" was observed live).
// checksDir must still find the node's own clone instead of erroring on an
// absolute/garbage Workdir it was never meant to consult.
func TestChecksDirIgnoresGarbageWorkdirWhenDeriving(t *testing.T) {
	cfg := testChecksConfig(t, nil, "/tmp")
	cfg.ChatID = "chat-1"
	cfg.NodeID = "impl_node"
	cfg.DeriveChecks = true
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, "impl_node/.git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok, err := checksDir(cfg)
	if err != nil {
		t.Fatalf("checksDir: %v (want it to ignore the garbage Workdir and find the node's own clone)", err)
	}
	if !ok {
		t.Fatal("checksDir found no repo: it should have located the node's own clone")
	}
	if !strings.HasSuffix(got, "impl_node") {
		t.Errorf("checksDir = %q, want the node's own clone (…/impl_node)", got)
	}
}

// A default-ON check_commands allowlist must be safe on a host WITHOUT the
// toolchain: deriveChecks additionally gates each candidate on its binary
// existing (toolchainPresent), so a missing `go` derives nothing instead of
// failing every node with exit 127.
func TestDeriveChecks_SkipsMissingToolchain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"go build", "go vet", "go test"}

	if got := deriveChecks(dir, allow); len(got) != 3 {
		t.Fatalf("with go on PATH, want 3 derived checks, got %v", got)
	}

	t.Setenv("PATH", t.TempDir()) // a PATH with no binaries at all
	if got := deriveChecks(dir, allow); len(got) != 0 {
		t.Fatalf("without the toolchain, want no derived checks, got %v", got)
	}
}

// A repo with more than one toolchain present (e.g. package.json + go.mod, as
// quack's own repo root has) must derive checks from ALL of them, not just the
// first match - see #349.
func TestDeriveChecks_UnionsMultipleToolchains(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"scripts": {"build": "tsc", "test": "vitest"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"npm run", "go build", "go vet", "go test"}

	got := deriveChecks(dir, allow)
	want := []string{"npm run build", "npm run test", "go build ./...", "go vet ./...", "go test ./..."}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want checks from both npm and go in order %v, got %v", want, got)
	}
}

// A single-ecosystem repo is unaffected: still just its own toolchain's checks.
func TestDeriveChecks_SingleEcosystemUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"go build", "go vet", "go test"}

	got := deriveChecks(dir, allow)
	want := []string{"go build ./...", "go vet ./...", "go test ./..."}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want only go checks %v, got %v", want, got)
	}
}

// #585: in a --depth 1 clone every dependency-needing check fails on the base
// commit, so all of them are waived and the gate keeps no deterministic teeth -
// which is how a PR that failed CI's gofmt shipped. gofmt needs no deps, no
// module cache and no network, so it is the one check that still runs there.
func TestDeriveChecks_IncludesGofmt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := deriveChecks(dir, []string{"go build", "gofmt"})
	var found bool
	for _, c := range got {
		if strings.HasPrefix(c, "gofmt") {
			found = true
		}
	}
	if !found {
		t.Errorf("derived %v, want a gofmt check among them", got)
	}
}

// A format check is NOT special-cased past failsAtBase. The first cut of #585
// marked it never-waivable on the premise that formatting violations cannot
// pre-exist; they can - `gofmt -l` lists every unformatted file in the tree, so
// on a repo carrying formatting debt that would gate a worker for someone
// else's mess, reintroducing exactly what #583 fixed. Waiving is correct there,
// and on a clean base (the common case) the check passes at base and still
// gates the worker's own violations.
func TestGofmtCheckIsWaivedWhenItAlreadyFailsAtBase(t *testing.T) {
	cfg, _ := clonedRepoConfig(t, []string{"gofmt -l . | wc -l | grep -q ^0$"}, map[string]string{
		// Committed unformatted: the debt exists on the base commit.
		"bad.go": "package x\nfunc  F()  {}\n",
	})
	sc, applies := checksPassCriterion(context.Background(), cfg)
	if applies && sc.Score == 0 {
		t.Errorf("gated on a gofmt violation that already exists at base - repo debt, not this worker's: %s", sc.Reason)
	}
}

// The other half: a violation the worker itself introduced DOES gate, because
// the check passes on the clean base commit. Without this the feature is a
// no-op - `gofmt -l` alone always exits 0, so the derived check pipes its count
// through grep to turn "nothing listed" into the exit status.
func TestGofmtCheckGatesAWorkerIntroducedViolation(t *testing.T) {
	cfg, repo := clonedRepoConfig(t, []string{"gofmt -l . | wc -l | grep -q ^0$"}, map[string]string{
		"ok.go": "package x\n\nfunc F() {}\n",
	})
	// The worker's own edit, uncommitted and unformatted.
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package x\nfunc  G()  {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, applies := checksPassCriterion(context.Background(), cfg)
	if !applies || sc.Score != 0 {
		t.Errorf("want the worker's own formatting violation to gate; got applies=%v score=%v %s", applies, sc.Score, sc.Reason)
	}
}

// TestDeriveChecksGradle pins #638: a Gradle/Android repo derived NOTHING, so
// the gate approved Kotlin that didn't compile (NightsOut#93). The wrapper is
// the marker every such repo ships.
func TestDeriveChecksGradle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := deriveChecks(dir, []string{"./gradlew"})
	if len(got) == 0 {
		t.Fatal("deriveChecks found no gradle checks - the gate would run nothing and pass anything")
	}
	var compiles bool
	for _, c := range got {
		if strings.Contains(c, "compile") {
			compiles = true
		}
	}
	if !compiles {
		t.Errorf("derived %v - want a compile check, the one that would have caught #93", got)
	}
}

// TestUnsupportedBuildSystemIsNamed pins the root-cause half of #638: a repo
// that plainly HAS a build system we can't derive for must be distinguishable
// from one with nothing to verify, so "gated on nothing" can be said out loud.
func TestUnsupportedBuildSystemIsNamed(t *testing.T) {
	for file, want := range map[string]string{"Cargo.toml": "cargo", "pom.xml": "maven", "pyproject.toml": "python"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := unsupportedBuildSystem(dir); got != want {
			t.Errorf("unsupportedBuildSystem(%s) = %q, want %q", file, got, want)
		}
	}
	if got := unsupportedBuildSystem(t.TempDir()); got != "" {
		t.Errorf("empty dir named %q - a repo with no build system must stay quiet", got)
	}
}

// TestChecksSkipNote_OnlyChangePropertiesEarnANote pins the #780 scope call:
// no_repo/no_checks_derived/unsupported_build_system are properties of the
// change (quack couldn't verify it) and earn a note; not_configured/
// no_workspace are operator config states and stay silent.
func TestChecksSkipNote_OnlyChangePropertiesEarnANote(t *testing.T) {
	cases := map[string]bool{
		skipReasonNotConfigured:    false,
		skipReasonNoWorkspace:      false,
		skipReasonNoRepo:           true,
		skipReasonNoChecksDerived:  true,
		skipReasonUnsupportedBuild: true,
	}
	for reason, wantNote := range cases {
		got := checksSkipNote(reason)
		if (got != "") != wantNote {
			t.Errorf("checksSkipNote(%q) = %q, want non-empty=%v", reason, got, wantNote)
		}
		if wantNote && !strings.Contains(got, reason) {
			t.Errorf("checksSkipNote(%q) = %q, want it to carry the exact skip_reason string", reason, got)
		}
	}
	if got := checksSkipNote("not_a_real_reason"); got != "" {
		t.Errorf("checksSkipNote of an unrecognised reason = %q, want empty (fail closed to silence)", got)
	}
}

// TestChecksSkipReason_SameStringAsSpanAndMetric pins #780 test case 4: the
// reason skipChecks hands to the span attribute (a stand-in for the
// otelobs.RecordChecksSkipped metric label - both are set from the same
// `reason` argument) is the identical string checksSkipNote puts in front of
// a reader, so the log, the metric, and the PR agree.
func TestChecksSkipReason_SameStringAsSpanAndMetric(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	cfg, root := scopeCfg(t, "", "cargo")
	mkRepo(t, root, map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"})

	got, ok := checksPassCriterionTraced(context.Background(), cfg)
	if ok {
		t.Fatal("checks_pass should not apply - Cargo.toml has no derivable check")
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	var spanReason string
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "skip_reason" {
			spanReason = a.Value.AsString()
		}
	}
	if spanReason != skipReasonUnsupportedBuild {
		t.Fatalf("span skip_reason = %q, want %q", spanReason, skipReasonUnsupportedBuild)
	}
	if got.Reason != spanReason {
		t.Fatalf("criterionScore.Reason = %q, want the same string the span/metric recorded (%q)", got.Reason, spanReason)
	}
	if note := checksSkipNote(got.Reason); !strings.Contains(note, spanReason) {
		t.Errorf("PR note %q does not carry the exact skip_reason string %q", note, spanReason)
	}
}
