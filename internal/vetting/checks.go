package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// probeChecksPass names this probe's execute_tool ledger event (emitProbeEvent,
// probeemit.go) - one per check command run via workspace.RunPipeline.
const probeChecksPass = "checks_pass"

// Skip reasons for checksPassCriterion's ok=false returns - see skipChecks.
const (
	skipReasonNotConfigured   = "not_configured"    // no cfg.Checks and DeriveChecks is off
	skipReasonNoWorkspace     = "no_workspace"      // no workspace wired up and nothing to derive from
	skipReasonNoRepo          = "no_repo"           // no single repo found to derive checks from
	skipReasonNoChecksDerived = "no_checks_derived" // a repo was found but nothing recognisable to check
	// skipReasonUnsupportedBuild is skipReasonNoChecksDerived's LOUD case: the
	// repo plainly has a build system we cannot derive checks for, so "nothing
	// verified" must not read as "verified and passed" (#638).
	skipReasonUnsupportedBuild = "unsupported_build_system"
)

// skipChecks records that the deterministic checks criterion did NOT apply -
// the case that matters for quack's phantom-success history (a fabricated
// exploration once scored 0.9 with nothing behind it; a phantom delivery
// shipped). ctx is checksPassCriterion's own ctx, which callers (see
// checksPassCriterionTraced) already pass in span-carrying, so the reason
// lands as an attribute on the gate.checks span AND a counter, both
// queryable in Tempo/Grafana - "checks passed" and "checks never ran" can no
// longer look identical.
func skipChecks(ctx context.Context, reason string) (criterionScore, bool) {
	oteltrace.SpanFromContext(ctx).SetAttributes(attribute.String("skip_reason", reason))
	otelobs.RecordChecksSkipped(reason)
	return criterionScore{}, false
}

// maxCheckOutputChars bounds ONE failing check's output as folded into the
// checks_pass Reason, which becomes revise-prompt feedback - unbounded build
// output re-accumulated each round once pushed the revise prompt past the model
// window. A compiler's FIRST errors are the actionable ones; the 40th cascade
// is noise. checksPassCriterion stops at the first failing check, so this also
// caps the TOTAL check-output contribution.
const maxCheckOutputChars = 2_000

// checksPassCriterion runs cfg.Checks, or checks DERIVED from the repo on disk
// when unset (the planner guesses before ever seeing the repo). Uses
// workspace.RunPipeline - argv-only, never a shell: `checks` is an operator
// allowlist (MatchesCheckPrefix), and a prefix allowlist means nothing if the
// suffix can open one. Weakest-link: one failing check scores 0; ok=false when
// nothing applies. Called from computeDeterministicCriteria (node.go).
func checksPassCriterion(ctx context.Context, cfg Config) (criterionScore, bool) {
	if len(cfg.Checks) == 0 && !cfg.DeriveChecks {
		return skipChecks(ctx, skipReasonNotConfigured)
	}
	if cfg.Workspace == nil {
		if len(cfg.Checks) == 0 {
			return skipChecks(ctx, skipReasonNoWorkspace) // nothing to derive from - not a failure
		}
		// A node with Checks set but no workspace wired up is a config/wiring
		// bug (internal/serve's buildAgents didn't stamp Workspace onto the
		// base Config), not a model or user error - fail closed rather than
		// running unjailed.
		return criterionScore{Score: 0, Reason: "deterministic: this node has checks configured but no workspace is wired up (internal error - contact the operator)"}, true
	}
	dir, ok, err := checksDir(cfg)
	if err != nil {
		return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: checks workdir %q: %v", cfg.Workdir, err)}, true
	}
	if !ok {
		// The planner omitted `workdir` and the node's workspace holds no single
		// repo to derive checks from. Skip the criterion rather than guess - a
		// node must never fail because the planner left a field out.
		slog.Info("no single repo found to derive checks from; skipping checks", "component", "vetting", "node", cfg.NodeID)
		return skipChecks(ctx, skipReasonNoRepo)
	}
	checks := cfg.Checks
	if len(checks) == 0 {
		checks = deriveChecks(dir, cfg.CheckCommands)
		if len(checks) == 0 {
			if bs := unsupportedBuildSystem(dir); bs != "" {
				// Loud: this node was gated on NOTHING. Silence here reads as a
				// clean bill of health and shipped non-compiling code once (#638).
				slog.Warn("repo has a build system but no checks could be derived; this node is gated on NOTHING",
					"component", "vetting", "node", cfg.NodeID, "dir", dir, "build_system", bs)
				return skipChecks(ctx, skipReasonUnsupportedBuild)
			}
			slog.Info("no checks derived from the repo; skipping checks", "component", "vetting", "node", cfg.NodeID, "dir", dir)
			return skipChecks(ctx, skipReasonNoChecksDerived)
		}
		slog.Info("derived checks from the repo", "component", "vetting", "node", cfg.NodeID, "dir", dir, "checks", checks)
	}
	caps := checksCaps(cfg)
	var preexisting []string
	for _, check := range checks {
		stages, err := workspace.SplitPipeline(check)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}, true
		}
		res, err := workspace.RunPipeline(ctx, dir, stages, caps)
		var probeResult map[string]any
		if err == nil {
			probeResult = map[string]any{"exit_code": res.ExitCode, "output": boundCheckOutput(res.Output)}
		}
		emitProbeEvent(ctx, probeChecksPass, map[string]any{"check": check}, probeResult, err)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}, true
		}
		if res.ExitCode != 0 {
			// The gate may only fail a node for what the node's own change BROKE.
			// A check that already fails on the repo's base commit is repo debt the
			// worker cannot fix and is not responsible for (baseline.go).
			if failsAtBase(dir, check, caps) {
				slog.Warn("check already fails at base; not gating on it", "component", "vetting", "node", cfg.NodeID, "check", check)
				preexisting = append(preexisting, check)
				continue
			}
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: check %q failed (exit %d):\n%s%s", check, res.ExitCode, boundCheckOutput(res.Output), preexistingNote(preexisting))}, true
		}
	}
	// If ALL derived checks were waived this is materially different from
	// "checks passed" — the gate verified nothing deterministic. Callers and
	// the human viewer should see a warning rather than conflating two states.
	if len(cfg.Checks) == 0 && cfg.DeriveChecks && len(preexisting) == len(checks) && len(checks) > 0 {
		slog.Warn("all derived checks waived for this node; no deterministic verification ran", "component", "vetting", "node", cfg.NodeID)
	}
	return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d check(s) passed%s", len(checks), preexistingNote(preexisting))}, true
}

// preexistingNote is the context line naming the checks that were IGNORED
// because they already fail at the repo's base commit - so a worker reading the
// revise feedback isn't confused by a check it saw fail that nothing asked it to
// fix, and an operator reading the verdict sees the repo has debt.
func preexistingNote(checks []string) string {
	if len(checks) == 0 {
		return ""
	}
	return fmt.Sprintf("\n(ignored, not your fault: %s - already failing on the repo's base commit, before your change)",
		strings.Join(checks, ", "))
}

// boundCheckOutput keeps a failing check's output within maxCheckOutputChars
// (reusing the judge's head+tail boundExcerpt) and appends a marker naming how
// much was dropped, so the revise prompt carries the first, actionable errors
// without the cascade. See maxCheckOutputChars.
func boundCheckOutput(out string) string {
	if len(out) <= maxCheckOutputChars {
		return out
	}
	return boundExcerpt(out, maxCheckOutputChars) + fmt.Sprintf(
		"\n[check output truncated: %d of %d bytes shown - fix the FIRST errors, the rest are usually cascades; re-run the check yourself to see them all]",
		maxCheckOutputChars, len(out))
}

// checksCaps stamps the node's own directory as WorkRoot, so a sandboxed
// check child mounts the workspace exactly where the worker's shell and fs
// tools see it - otherwise a compiler's absolute paths would name a THIRD
// spelling of the file the revise feedback asks the worker to fix. Left
// unset if the workspace dir was never written; childArgv falls back to cwd.
func checksCaps(cfg Config) workspace.Caps {
	caps := cfg.WorkspaceCaps
	root, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.NodeDir(cfg.NodeID))
	if err != nil {
		return caps
	}
	caps.WorkRoot = root
	return caps
}

// checksDir returns the directory a node's checks run in: the planner's
// Workdir when Checks is explicit, else the ONE repo (dir holding .git),
// found by searching the node's own dir first, then the chat scope root.
// Node-first matters - from the chat root the search sees every CONCURRENT
// node's clone, finds more than one repo, and skips checks entirely.
func checksDir(cfg Config) (string, bool, error) {
	// Workdir is documented as ignored when Checks is empty (dag.Node.Workdir,
	// Config.Workdir) - the planner is told to set it only alongside explicit
	// checks, but a model doesn't always honor that (#620: "/tmp" reached here
	// on a derive-only node and the jail rejected it before any repo search ran).
	workdir := cfg.Workdir
	if len(cfg.Checks) == 0 {
		workdir = ""
	}
	nodeStart, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, joinWritten(workspace.NodeDir(cfg.NodeID), workdir))
	if err != nil {
		return "", false, err
	}
	chatStart, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workdir)
	if err != nil {
		return "", false, err
	}
	if len(cfg.Checks) > 0 {
		// Explicit planner-set checks keep their historical meaning: run exactly
		// where the planner said. Fail closed - a workdir that exists nowhere is
		// still returned, so the check errors rather than being silently skipped.
		if nodeStart != chatStart && !isDir(nodeStart) && isDir(chatStart) {
			return chatStart, true, nil
		}
		return nodeStart, true, nil
	}
	for _, start := range []string{nodeStart, chatStart} {
		if repos := workspace.FindRepos(start); len(repos) == 1 {
			return repos[0], true, nil
		}
		if nodeStart == chatStart {
			break
		}
	}
	return "", false, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// npmCheckScripts are the package.json script names worth gating on, in run
// order. ONLY scripts the repo actually declares are used - a repo whose
// typecheck is `next build` must never be handed an invented `npx tsc`.
var npmCheckScripts = []string{"build", "typecheck", "type-check", "check-types", "lint", "test"}

// makeCheckTargets are the Makefile targets worth gating on, in run order.
var makeCheckTargets = []string{"build", "lint", "test"}

// makeTargetRe matches a Makefile target definition line ("build:", "test: dep"),
// excluding variable assignments ("FOO := bar").
var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_./-]+)\s*:(?:[^=]|$)`)

// deriveChecks reads dir - the repo the node worked in - and returns its OWN
// check commands, filtered by the workspace.check_commands allowlist (empty
// allowlist disables checks). UNION of every toolchain present, npm → go →
// make order - not just the first found. Returning none is normal, not a
// failure.
func deriveChecks(dir string, allow []string) []string {
	var cands []string
	if fileExists(dir, "package.json") {
		scripts := packageScripts(dir)
		for _, s := range npmCheckScripts {
			if scripts[s] {
				cands = append(cands, "npm run "+s)
			}
		}
	}
	if fileExists(dir, "go.mod") {
		cands = append(cands, "go build ./...", "go vet ./...", "go test ./...")
		// `gofmt -l` LISTS unformatted files but always exits 0, so it cannot
		// gate anything on its own - pipe the count through grep to turn "no
		// files listed" into the exit status (CI's own step wraps it in
		// `test -z` for the same reason). Only pipes, no substitution:
		// workspace.SplitPipeline supports the former, not the latter.
		cands = append(cands, "gofmt -l . | wc -l | grep -q ^0$")
	}
	// Gradle: the wrapper is the entry point every Android/Kotlin repo ships,
	// and compiling is the check that matters most - the gate approved
	// non-compiling Kotlin because none of these existed (#638).
	if fileExists(dir, "gradlew") {
		cands = append(cands, "./gradlew compileDebugKotlin", "./gradlew testDebugUnitTest")
	}
	if fileExists(dir, "Makefile") {
		targets := makeTargets(dir)
		for _, t := range makeCheckTargets {
			if targets[t] {
				cands = append(cands, "make "+t)
			}
		}
	}
	var out []string
	for _, c := range cands {
		if workspace.MatchesCheckPrefix(c, allow) && toolchainPresent(dir, c) {
			out = append(out, c)
		}
	}
	// npx prettier --check is a derived candidate for any JS/TS repo when the
	// binary exists and the prefix is in the allowlist. Unlike npm-run scripts
	// (which must be declared by the repo), Prettier is an external formatter
	// whose presence alone signals intent — if npx is on PATH and "npx prettier"
	// is allowed, run it. Never waive: formatting violations cannot pre-exist.
	if fileExists(dir, "package.json") && toolchainPresent(dir, "npx prettier") && workspace.MatchesCheckPrefix("npx prettier", allow) {
		out = append(out, "npx prettier --check")
	}
	return out
}

// unsupportedBuildSystem names the build system a repo obviously HAS when
// deriveChecks produced nothing for it - the difference between "there is
// nothing to verify here" and "we verified nothing". Markers deliberately
// include systems deriveChecks does not support: that is the whole point.
func unsupportedBuildSystem(dir string) string {
	for _, m := range []struct{ file, name string }{
		{"Cargo.toml", "cargo"},
		{"pom.xml", "maven"},
		{"build.gradle", "gradle"},
		{"build.gradle.kts", "gradle"},
		{"pyproject.toml", "python"},
		{"setup.py", "python"},
		{"Gemfile", "bundler"},
		{"composer.json", "composer"},
		{"CMakeLists.txt", "cmake"},
		{"BUILD.bazel", "bazel"},
	} {
		if fileExists(dir, m.file) {
			return m.name
		}
	}
	return ""
}

// toolchainPresent reports whether a derived check's binary exists - a bare
// name (go, npm, make) via the server's ambient PATH, a repo-relative one
// (./gradlew) via dir - exactly as workspace.RunPipeline will resolve it
// (workspace.ResolveExecutable, the same lookup newChildCmd uses). This is
// what makes a default-ON check_commands allowlist safe: a host without
// go/npm, or a repo without a gradlew wrapper, derives no such checks instead
// of failing every node with exit 127.
func toolchainPresent(dir, check string) bool {
	f := strings.Fields(check)
	if len(f) == 0 {
		return false
	}
	_, err := workspace.ResolveExecutable(dir, f[0])
	return err == nil
}

// packageScripts returns the set of script names declared in dir/package.json.
func packageScripts(dir string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil
	}
	set := make(map[string]bool, len(pkg.Scripts))
	for name := range pkg.Scripts {
		set[strings.TrimSpace(name)] = true
	}
	return set
}

// makeTargets returns the set of targets declared in dir/Makefile.
func makeTargets(dir string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, m := range makeTargetRe.FindAllStringSubmatch(string(raw), -1) {
		set[m[1]] = true
	}
	return set
}
