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

// probeChecksPass: ledger event name for checks-pass probe (emitProbeEvent).
const probeChecksPass = "checks_pass"

// Skip reasons for checksPassCriterion's ok=false returns.
const (
	skipReasonNotConfigured    = "not_configured"
	skipReasonNoWorkspace      = "no_workspace"
	skipReasonNoRepo           = "no_repo"
	skipReasonNoChecksDerived  = "no_checks_derived"
	skipReasonUnsupportedBuild = "unsupported_build_system"
)

// skipChecks records that the deterministic checks criterion did not apply.
// The reason rides back in criterionScore.Reason (ignored by callers that only
// check ok) so a passing node can still say why nothing was verified - same
// string the span attribute and metric record (#780).
func skipChecks(ctx context.Context, reason string) (criterionScore, bool) {
	oteltrace.SpanFromContext(ctx).SetAttributes(attribute.String("skip_reason", reason))
	otelobs.RecordChecksSkipped(reason)
	return criterionScore{Reason: reason}, false
}

// checksSkipNoteReasons: skip reasons that are a property of the CHANGE
// (repo state quack could not verify), not operator config - worth
// surfacing on a passing node. not_configured/no_workspace describe how the
// operator set the node up, not anything about this change, so they stay
// silent (#780).
var checksSkipNoteReasons = map[string]bool{
	skipReasonNoRepo:           true,
	skipReasonNoChecksDerived:  true,
	skipReasonUnsupportedBuild: true,
}

// checksSkipNote composes the passing-node caveat for a skip reason worth
// surfacing, or "" when the reason doesn't qualify (or checks ran, reason ==
// ""). Embeds the exact skip_reason string RecordChecksSkipped records, so
// the log, the metric, and this note agree.
func checksSkipNote(reason string) string {
	if !checksSkipNoteReasons[reason] {
		return ""
	}
	return fmt.Sprintf(
		"quack did not run a build/test check on this change (skip_reason: %s). "+
			"That says nothing about the change itself - it means quack has no compiled/tested confirmation of it, so treat \"passed\" here as judged, not verified.",
		reason)
}

// maxCheckOutputChars caps failing check output in revise-prompt feedback.
const maxCheckOutputChars = 2_000

// checksPassCriterion runs cfg.Checks or derived checks. Workspace.RunPipeline - argv-only. Weakest-link.
func checksPassCriterion(ctx context.Context, cfg Config) (criterionScore, bool) {
	if len(cfg.Checks) == 0 && !cfg.DeriveChecks {
		return skipChecks(ctx, skipReasonNotConfigured)
	}
	if cfg.Workspace == nil {
		if len(cfg.Checks) == 0 {
			return skipChecks(ctx, skipReasonNoWorkspace) // nothing to derive from - not a failure
		}
		// Checks set but no workspace wired: fail closed (config bug).
		return criterionScore{Score: 0, Reason: "deterministic: this node has checks configured but no workspace is wired up (internal error - contact the operator)"}, true
	}
	dir, ok, err := checksDir(cfg)
	if err != nil {
		return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: checks workdir %q: %v", cfg.Workdir, err)}, true
	}
	if !ok {
		// Planner omitted workdir and no single repo to derive from - skip rather than fail on a planner omission.
		slog.Info("no single repo found to derive checks from; skipping checks", "component", "vetting", "node", cfg.NodeID)
		return skipChecks(ctx, skipReasonNoRepo)
	}
	checks := cfg.Checks
	if len(checks) == 0 {
		checks = deriveChecks(dir, cfg.CheckCommands)
		if len(checks) == 0 {
			if bs := unsupportedBuildSystem(dir); bs != "" {
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
			// Don't gate on pre-existing failures in the base commit (baseline.go).
			if failsAtBase(dir, check, caps) {
				slog.Warn("check already fails at base; not gating on it", "component", "vetting", "node", cfg.NodeID, "check", check)
				preexisting = append(preexisting, check)
				continue
			}
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: check %q failed (exit %d):\n%s%s", check, res.ExitCode, boundCheckOutput(res.Output), preexistingNote(preexisting))}, true
		}
	}
	// All derived checks waived - materially different from "checks passed".
	if len(cfg.Checks) == 0 && cfg.DeriveChecks && len(preexisting) == len(checks) && len(checks) > 0 {
		slog.Warn("all derived checks waived for this node; no deterministic verification ran", "component", "vetting", "node", cfg.NodeID)
	}
	return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d check(s) passed%s", len(checks), preexistingNote(preexisting))}, true
}

// preexistingNote names checks ignored because they fail at base commit.
func preexistingNote(checks []string) string {
	if len(checks) == 0 {
		return ""
	}
	return fmt.Sprintf("\n(ignored, not your fault: %s - already failing on the repo's base commit, before your change)",
		strings.Join(checks, ", "))
}

// boundCheckOutput caps failing check output (reuses boundExcerpt).
func boundCheckOutput(out string) string {
	if len(out) <= maxCheckOutputChars {
		return out
	}
	return boundExcerpt(out, maxCheckOutputChars) + fmt.Sprintf(
		"\n[check output truncated: %d of %d bytes shown - fix the FIRST errors, the rest are usually cascades; re-run the check yourself to see them all]",
		maxCheckOutputChars, len(out))
}

// checksCaps stamps the node's own directory as WorkRoot for sandbox consistency.
func checksCaps(cfg Config) workspace.Caps {
	caps := cfg.WorkspaceCaps
	root, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.NodeDir(cfg.NodeID))
	if err != nil {
		return caps
	}
	caps.WorkRoot = root
	return caps
}

// checksDir: the planner's Workdir (explicit checks) or the one repo dir (derived). Node-first to avoid sibling clones.
func checksDir(cfg Config) (string, bool, error) {
	// Workdir ignored when Checks is empty (planner sometimes sets it anyway).
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
		// Run explicit checks where the planner said. Fail closed on missing workdir.
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

// npmCheckScripts: package.json script names to gate on, in run order.
var npmCheckScripts = []string{"build", "typecheck", "type-check", "check-types", "lint", "test"}

// makeCheckTargets: Makefile targets to gate on, in run order.
var makeCheckTargets = []string{"build", "lint", "test"}

// makeTargetRe matches Makefile target lines, excluding variable assignments.
var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_./-]+)\s*:(?:[^=]|$)`)

// deriveChecks returns the repo's own check commands filtered by the allowlist.
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
		// gofmt -l exits 0 always; pipe count through grep to get exit status.
		cands = append(cands, "gofmt -l . | wc -l | grep -q ^0$")
	}
	// Gradle: compile is the critical check the gate previously missed (#638).
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
	// npx prettier --check: derived for JS/TS repos where the binary exists and the prefix is allowed.
	if fileExists(dir, "package.json") && toolchainPresent(dir, "npx prettier") && workspace.MatchesCheckPrefix("npx prettier", allow) {
		out = append(out, "npx prettier --check")
	}
	return out
}

// unsupportedBuildSystem names the build system when deriveChecks found nothing.
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

// toolchainPresent reports whether a derived check's binary exists (ambient PATH or repo-relative).
func toolchainPresent(dir, check string) bool {
	f := strings.Fields(check)
	if len(f) == 0 {
		return false
	}
	_, err := workspace.ResolveExecutable(dir, f[0])
	return err == nil
}

// packageScripts returns script names from dir/package.json.
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
