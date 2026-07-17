package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// maxCheckOutputChars bounds ONE failing check's output as folded into the
// checks_pass Reason, which becomes revise-prompt feedback — unbounded build
// output re-accumulated each round once pushed the revise prompt past the model
// window. A compiler's FIRST errors are the actionable ones; the 40th cascade
// is noise. checksPassCriterion stops at the first failing check, so this also
// caps the TOTAL check-output contribution.
const maxCheckOutputChars = 2_000

// checksPassCriterion is the GATE side of §4 (deterministic gates): it runs the
// node's checks — cfg.Checks when the planner set them (an explicit override,
// already plan-time validated argv-safe), else the checks DERIVED from the repo
// on disk (deriveChecks) — via workspace.RunPipeline, argv-only and never a
// shell (pipes are native; everything else a shell would interpret stays
// unexpressible). This is deliberately NOT run_command's runner: `checks` is an
// operator allowlist (MatchesCheckPrefix), and a prefix allowlist means nothing
// if the suffix can open a shell — run_command hands its line to a real shell
// instead (RunShell, #277). Stops at the first failure.
//
// Why derive: the planner authors the DAG BEFORE anything has looked at the
// repo, so any checks it writes are guesses (`go build` for a JavaScript repo).
// Check commands are a property of the REPO and are discovered from it here, at
// gate time, once the worker has cloned it.
//
// Called from foldDeterministic (node.go) exactly like grounded_in_retrieval: a
// failing check folds in as criterion `checks_pass` with Score 0 (weakest-link —
// one failing check sinks the round on its own) and a Reason naming the command
// plus a BOUNDED head of its output, so composeFeedback (node.go) carries the
// actual compiler/test failure into the revise prompt without blowing its budget.
// All checks passing scores 1. ok=false means the criterion does not apply at all
// (no checks and nothing to derive them from) — the node is then untouched by it,
// exactly as a research or synthesis node is.
func checksPassCriterion(ctx context.Context, cfg Config) (criterionScore, bool) {
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
	caps := checksCaps(cfg)
	var preexisting []string
	for _, check := range checks {
		stages, err := workspace.SplitPipeline(check)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}, true
		}
		res, err := workspace.RunPipeline(ctx, dir, stages, caps)
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
	return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d check(s) passed%s", len(checks), preexistingNote(preexisting))}, true
}

// preexistingNote is the context line naming the checks that were IGNORED
// because they already fail at the repo's base commit — so a worker reading the
// revise feedback isn't confused by a check it saw fail that nothing asked it to
// fix, and an operator reading the verdict sees the repo has debt.
func preexistingNote(checks []string) string {
	if len(checks) == 0 {
		return ""
	}
	return fmt.Sprintf("\n(ignored, not your fault: %s — already failing on the repo's base commit, before your change)",
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
		"\n[check output truncated: %d of %d bytes shown — fix the FIRST errors, the rest are usually cascades; re-run the check yourself to see them all]",
		maxCheckOutputChars, len(out))
}

// checksCaps stamps the node's OWN directory onto the caps the checks run with,
// so a sandboxed check child sees the workspace exactly where the worker's shell
// and the fs tools see it: at workspace.SandboxWorkRoot, with the repo one level
// under it. Without it the check's cwd would be mounted as the root, and a
// compiler's absolute paths — which land verbatim in the revise feedback the
// worker then reads — would name a THIRD spelling of the file it is being asked
// to fix. One namespace means every child of a node speaks it.
//
// A node whose workspace dir does not exist (nothing was ever written) leaves
// WorkRoot unset; childArgv falls back to the check's own cwd, exactly as before.
func checksCaps(cfg Config) workspace.Caps {
	caps := cfg.WorkspaceCaps
	root, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.NodeDir(cfg.NodeID))
	if err != nil {
		return caps
	}
	caps.WorkRoot = root
	return caps
}

// checksDir returns the absolute directory a node's checks run in: the node's
// Workdir when the planner set explicit Checks to run there, else — when checks
// are being DERIVED — the ONE repo (a directory holding a .git) at or beneath the
// node's Workdir (the workspace scope root when it set none). ok=false when no
// single repo can be located: skip the checks rather than guess which of several
// trees is "the" repo.
//
// The search matters: the planner may legitimately set no workdir, and the repo
// usually sits one level down where git_clone put it — resolving a workdir is
// not the same as FINDING the repo, so we find it (a scope-root fallback once
// derived no checks at all and let non-compiling code pass).
//
// Workdir resolves against the node's OWN working dir first (<chat>/<node>/…,
// where the node's git_clone landed its repo — exactly as the worker's own tools
// resolve it), then against the chat scope root as a fallback (an un-gated node,
// or a worker that used the "/" escape hatch to work at the root). Node-first is
// what keeps a plan's CONCURRENT nodes from each seeing several repos — from the
// chat root the search sees every node's clone, finds no single repo, and gates
// nothing.
func checksDir(cfg Config) (string, bool, error) {
	nodeStart, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, joinWritten(workspace.NodeDir(cfg.NodeID), cfg.Workdir))
	if err != nil {
		return "", false, err
	}
	chatStart, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, cfg.Workdir)
	if err != nil {
		return "", false, err
	}
	if len(cfg.Checks) > 0 {
		// Explicit planner-set checks keep their historical meaning: run exactly
		// where the planner said. Fail closed — a workdir that exists nowhere is
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
		if workspace.MatchesCheckPrefix(c, allow) && toolchainPresent(c) {
			out = append(out, c)
		}
	}
	return out
}

// toolchainPresent reports whether a derived check's binary exists on the
// server (the ambient PATH — the same lookup RunArgv resolves argv[0] with).
// This is what makes a default-ON check_commands allowlist safe: a host
// without go/npm derives no go/npm checks instead of failing every node with
// exit 127s.
func toolchainPresent(check string) bool {
	f := strings.Fields(check)
	if len(f) == 0 {
		return false
	}
	_, err := exec.LookPath(f[0])
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
