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

	"github.com/fagerbergj/quack/internal/workspace"
)

// maxCheckOutputChars bounds ONE failing check's output as folded into the
// checks_pass Reason — which becomes the revise prompt's feedback section
// (composeFeedback → buildRevisionContent). Regression (live e2e 2026-07-12):
// a failing `npx tsc`/`npm run build` dumps a dozen multi-line errors and a
// whole build trace; folded in whole and re-accumulated each round it pushed
// the revise prompt past the model window, compaction truncated the worker's
// OWN task prompt as a last resort, and the worker lost its state and failed
// outright by round 5. A compiler's FIRST errors are the actionable ones; the
// 40th cascade is noise — ~2KB is plenty. checksPassCriterion stops at the
// first failing check, so this also caps the TOTAL check-output contribution.
const maxCheckOutputChars = 2_000

// checksPassCriterion is the GATE side of §4 (deterministic gates): it runs the
// node's checks — cfg.Checks when the planner set them (an explicit override,
// already plan-time validated argv-safe), else the checks DERIVED from the repo
// on disk (deriveChecks) — via the SAME jailed runner run_command uses
// (workspace.RunPipeline; pipes are native, everything else a shell would
// interpret stays unexpressible), stopping at the first failure.
//
// Why derive: the planner authors the DAG BEFORE anything has looked at the repo,
// so any checks it writes are guesses (live e2e 2026-07-12: `go build` for a
// JavaScript repo, `npx tsc` for a repo whose typecheck is `next build`). Check
// commands are a property of the REPO and are discovered from it here, at gate
// time, once the worker has cloned it.
//
// Called from foldDeterministic (node.go) exactly like grounded_in_retrieval: a
// failing check folds in as criterion `checks_pass` with Score 0 (weakest-link —
// one failing check sinks the round on its own) and a Reason naming the command
// plus a BOUNDED head of its output, so composeFeedback (node.go) carries the
// actual compiler/test failure into the revise prompt without blowing its budget.
// All checks passing scores 1. ok=false means the criterion does not apply at all
// (no checks and nothing to derive them from) — the node is then untouched by it,
// exactly as a research or synthesis node is.
func checksPassCriterion(cfg Config) (criterionScore, bool) {
	if len(cfg.Checks) == 0 && !cfg.DeriveChecks {
		return criterionScore{}, false
	}
	if cfg.Workspace == nil {
		if len(cfg.Checks) == 0 {
			return criterionScore{}, false // nothing to derive from — not a failure
		}
		// A node with Checks set but no workspace wired up is a config/wiring
		// bug (internal/serve's buildAgents didn't stamp Workspace onto the
		// base Config), not a model or user error — fail closed rather than
		// running unjailed.
		return criterionScore{Score: 0, Reason: "deterministic: this node has checks configured but no workspace is wired up (internal error — contact the operator)"}, true
	}
	dir, ok, err := checksDir(cfg)
	if err != nil {
		return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: checks workdir %q: %v", cfg.Workdir, err)}, true
	}
	if !ok {
		// The planner omitted `workdir` and the node's workspace holds no single
		// repo to derive checks from. Skip the criterion rather than guess — a
		// node must never fail because the planner left a field out.
		slog.Info("no single repo found to derive checks from; skipping checks", "component", "vetting", "node", cfg.NodeID)
		return criterionScore{}, false
	}
	checks := cfg.Checks
	if len(checks) == 0 {
		checks = deriveChecks(dir, cfg.CheckCommands)
		if len(checks) == 0 {
			slog.Info("no checks derived from the repo; skipping checks", "component", "vetting", "node", cfg.NodeID, "dir", dir)
			return criterionScore{}, false
		}
		slog.Info("derived checks from the repo", "component", "vetting", "node", cfg.NodeID, "dir", dir, "checks", checks)
	}
	for _, check := range checks {
		stages, err := workspace.SplitPipeline(check)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}, true
		}
		res, err := workspace.RunPipeline(context.Background(), dir, stages, cfg.WorkspaceCaps)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}, true
		}
		if res.ExitCode != 0 {
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: check %q failed (exit %d):\n%s", check, res.ExitCode, boundCheckOutput(res.Output))}, true
		}
	}
	return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d check(s) passed", len(checks))}, true
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
		"\n[check output truncated: %d of %d bytes shown — fix the FIRST errors, the rest are usually cascades; re-run the check yourself to see them all]",
		maxCheckOutputChars, len(out))
}

// checksDir returns the absolute directory a node's checks run in: the node's
// Workdir when the planner set one (Jail.Resolve, per-chat scope), else — when
// checks are being derived — the ONE repo (a directory holding a .git) in the
// node's workspace scope. ok=false when no single repo can be located: skip the
// checks rather than guess which of several trees is "the" repo.
func checksDir(cfg Config) (string, bool, error) {
	if cfg.Workdir != "" || len(cfg.Checks) > 0 {
		// Explicit Checks with no Workdir keep their historical meaning: run in
		// the scope root itself.
		dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, cfg.Workdir)
		return dir, err == nil, err
	}
	root, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, "")
	if err != nil {
		return "", false, err
	}
	repos := findRepos(root)
	if len(repos) != 1 {
		return "", false, nil
	}
	return repos[0], true, nil
}

// findRepos returns the git repositories directly under root (or root itself) —
// where git_clone puts them (<scope>/<dir>).
func findRepos(root string) []string {
	if isRepo(root) {
		return []string{root}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && isRepo(filepath.Join(root, e.Name())) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

func isRepo(dir string) bool { return fileExists(dir, ".git") }

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// npmCheckScripts are the package.json script names worth gating on, in run
// order. ONLY scripts the repo actually declares are used — a repo whose
// typecheck is `next build` must never be handed an invented `npx tsc`.
var npmCheckScripts = []string{"build", "typecheck", "type-check", "check-types", "lint", "test"}

// makeCheckTargets are the Makefile targets worth gating on, in run order.
var makeCheckTargets = []string{"build", "lint", "test"}

// makeTargetRe matches a Makefile target definition line ("build:", "test: dep"),
// excluding variable assignments ("FOO := bar").
var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_./-]+)\s*:(?:[^=]|$)`)

// deriveChecks reads dir — the repo the node worked in — and returns the repo's
// OWN check commands, filtered by the configured workspace.check_commands
// allowlist (the security boundary stays exactly where it was; an empty allowlist
// means checks are disabled, so this returns nothing). First match wins:
// package.json (its declared scripts) → go.mod → Makefile (its declared targets)
// → none. Returning none is normal, not a failure: an unrecognised repo simply
// gets no deterministic gate.
func deriveChecks(dir string, allow []string) []string {
	var cands []string
	switch {
	case fileExists(dir, "package.json"):
		scripts := packageScripts(dir)
		for _, s := range npmCheckScripts {
			if scripts[s] {
				cands = append(cands, "npm run "+s)
			}
		}
	case fileExists(dir, "go.mod"):
		cands = []string{"go build ./...", "go vet ./...", "go test ./..."}
	case fileExists(dir, "Makefile"):
		targets := makeTargets(dir)
		for _, t := range makeCheckTargets {
			if targets[t] {
				cands = append(cands, "make "+t)
			}
		}
	}
	var out []string
	for _, c := range cands {
		if workspace.MatchesCheckPrefix(c, allow) {
			out = append(out, c)
		}
	}
	return out
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
