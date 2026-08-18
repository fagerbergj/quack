package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SandboxProbeStatus is one probe's verdict: PASS/FAIL drive `check`'s exit
// code, INFO never does (a probe about a nice-to-have, e.g. nested-namespace
// availability, not a guarantee the jail makes).
type SandboxProbeStatus string

const (
	ProbePass SandboxProbeStatus = "PASS"
	ProbeFail SandboxProbeStatus = "FAIL"
	ProbeInfo SandboxProbeStatus = "INFO"
)

// SandboxProbeResult is one row of `quack sandbox check`'s table.
type SandboxProbeResult struct {
	Name     string
	Status   SandboxProbeStatus
	Evidence string
}

// SandboxRunner is the seam `check`'s probes run a shell command through -
// satisfied by a real seat (sh -c "$CMD" wrapped through WrapArgv/spawnEnv,
// see cmd/quack/sandbox.go) or a fake for unit tests.
type SandboxRunner interface {
	// Run runs sh -c script in the seat, returning combined stdout+stderr and
	// the exit code (0 on success).
	Run(ctx context.Context, script string) (output string, exitCode int, err error)
}

// sandboxProbe is one check_setup-style probe: runs a script, then classifies
// the result. ok's report is what SandboxProbeResult.Evidence renders.
type sandboxProbe struct {
	name string
	skip bool // true when the environment can't meaningfully run this probe (e.g. --mode none for a boundary probe)
	info bool // true when a FAIL should render INFO, not FAIL (doesn't affect check's exit code either way)
	// argv is the sh -c script to run.
	script string
}

// RunSandboxChecks runs every built-in probe through r and returns the
// table. checkCommands are the config's workspace.check_commands entries
// (each probed for presence on ChildPath - the issue body's "one probe per
// check_commands entry"). enforced is whether the resolved mode OS-enforces a
// boundary (workspace.EnforcesBoundary) - under `none` the boundary probes
// (EACCES-on-write, clone-denied-without-boundary) degrade to INFO rather
// than FAIL, since there is no boundary to have failed.
func RunSandboxChecks(ctx context.Context, r SandboxRunner, readOnly, enforced bool, checkCommands []string) []SandboxProbeResult {
	probes := []sandboxProbe{
		{name: "write $TMPDIR", script: `f="$TMPDIR/quack-sandbox-probe-$$"; echo x > "$f" && rm -f "$f"`},
		{name: "write $TMPDIR/sub/dir", script: `d="$TMPDIR/quack-sandbox-probe-$$/sub/dir"; mkdir -p "$d" && echo x > "$d/f" && rm -rf "$TMPDIR/quack-sandbox-probe-$$"`},
		{name: "write $HOME", script: `f="$HOME/.quack-sandbox-probe-$$"; echo x > "$f" && rm -f "$f"`},
		{name: "write cwd", script: `f="./.quack-sandbox-probe-$$"; echo x > "$f" && rm -f "$f"`, info: !enforced},
		{name: "go env GOTOOLCHAIN/GOMODCACHE/GOTMPDIR/GOCACHE", script: `go env GOTOOLCHAIN GOMODCACHE GOTMPDIR GOCACHE | grep -qv '^$' && for d in $(go env GOMODCACHE GOTMPDIR GOCACHE); do mkdir -p "$d" && [ -w "$d" ] || exit 1; done`},
		{name: "go build offline", script: goBuildProbeScript},
		{name: "git init+commit+push to bare $TMPDIR (no EXDEV)", script: gitPushProbeScript},
		{name: "git clone --local into $TMPDIR (hardlink path)", script: gitCloneLocalProbeScript},
		{name: "git push https://github.com/x/y (no credential prompt)", script: `GIT_TERMINAL_PROMPT=0 timeout 5 git push https://github.com/x/y HEAD:refs/heads/probe 2>&1; test $? -ne 0`},
		{name: "unshare --user true (nested userns)", script: `unshare --user true`, info: true},
		{name: "bwrap --version (nesting)", script: `bwrap --version`, info: true},
	}
	for _, cc := range checkCommands {
		bin := strings.Fields(cc)
		if len(bin) == 0 {
			continue
		}
		probes = append(probes, sandboxProbe{
			name:   "check_commands: " + cc + " on PATH",
			script: `command -v ` + shQuote(bin[0]),
			info:   true,
		})
	}

	results := make([]SandboxProbeResult, 0, len(probes))
	for _, p := range probes {
		results = append(results, runOneSandboxProbe(ctx, r, p, readOnly))
	}
	return results
}

func runOneSandboxProbe(ctx context.Context, r SandboxRunner, p sandboxProbe, readOnly bool) SandboxProbeResult {
	script := p.script
	// The cwd-write probe expects success unless the agent is read-only, in
	// which case EACCES is the PASS condition (issue body: "cwd EACCES iff
	// read-only").
	wantFail := p.name == "write cwd" && readOnly

	out, code, err := r.Run(ctx, script)
	evidence := strings.TrimSpace(out)
	if evidence == "" && err != nil {
		evidence = err.Error()
	}
	if evidence == "" {
		evidence = "exit " + strconv.Itoa(code)
	}
	if len(evidence) > 200 {
		evidence = evidence[:200] + "…"
	}

	pass := code == 0
	if wantFail {
		pass = code != 0
	}
	status := ProbeFail
	if pass {
		status = ProbePass
	} else if p.info {
		status = ProbeInfo
	}
	return SandboxProbeResult{Name: p.name, Status: status, Evidence: evidence}
}

// AnyFail reports whether results contains a FAIL - `check`'s exit code.
func AnyFail(results []SandboxProbeResult) bool {
	for _, r := range results {
		if r.Status == ProbeFail {
			return true
		}
	}
	return false
}

// FormatSandboxProbeTable renders results as the human-readable table `check` prints.
func FormatSandboxProbeTable(results []SandboxProbeResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%-5s %-55s %s\n", r.Status, r.Name, r.Evidence)
	}
	return b.String()
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// goBuildProbeScript builds a throwaway one-file module under $TMPDIR and
// builds it with GOFLAGS=-mod=mod, network disabled - the issue body's
// "go build ./... in a copy of a tiny Go module under $TMPDIR, offline".
const goBuildProbeScript = `
set -e
d="$TMPDIR/quack-sandbox-gobuild-$$"
mkdir -p "$d"
trap 'rm -rf "$d"' EXIT
cd "$d"
cat > go.mod <<'EOF'
module quackprobe

go 1.21
EOF
cat > main.go <<'EOF'
package main

func main() {}
EOF
GOFLAGS=-mod=mod GOPROXY=off go build ./...
`

// gitPushProbeScript proves git init+commit+push to a bare repo under
// $TMPDIR works with no EXDEV (the hardlink-across-devices failure #936
// chased) - both repos live under $TMPDIR so this only proves same-device
// git via push; gitCloneLocalProbeScript covers the clone --local hardlink path.
const gitPushProbeScript = `
set -e
base="$TMPDIR/quack-sandbox-gitpush-$$"
mkdir -p "$base"
trap 'rm -rf "$base"' EXIT
git init --bare -q "$base/bare.git"
git init -q "$base/work"
cd "$base/work"
git config user.email probe@quack.local
git config user.name probe
echo x > f
git add f
git commit -qm probe
git push -q "$base/bare.git" HEAD:refs/heads/probe
`

// gitCloneLocalProbeScript proves `git clone --local` (the hardlink path) works
// into $TMPDIR. Self-contained: cwd may not be a repo (a fresh --cwd is not),
// so it inits one under $TMPDIR and clones that - same-device hardlinks are
// what the probe is for, not cwd's identity.
const gitCloneLocalProbeScript = `
set -e
base="$TMPDIR/quack-sandbox-clonelocal-$$"
mkdir -p "$base"
trap 'rm -rf "$base"' EXIT
git init -q "$base/src"
git -C "$base/src" -c user.name=probe -c user.email=probe@quack commit -q --allow-empty -m probe
git clone --local -q "$base/src" "$base/dst"
`
