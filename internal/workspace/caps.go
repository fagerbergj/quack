package workspace

import "time"

// Caps bounds every workspace tool call. A cap is never a hard failure for a
// read-shaped result (read_file/list_dir/glob/grep all carry a `truncated`
// bool and truncate loudly instead) — see each tool's doc comment for exactly
// how its cap applies.
type Caps struct {
	// MaxReadBytes caps how much of a file read_file returns per call.
	MaxReadBytes int64
	// MaxWriteBytes caps how much content write_file accepts per call.
	MaxWriteBytes int64
	// MaxResults caps how many hits glob/grep return per call.
	MaxResults int
	// MaxListEntries caps how many entries list_dir returns per call.
	MaxListEntries int
	// Timeout bounds a single git/check/run_command invocation.
	Timeout time.Duration
	// MaxOutputBytes caps how much combined stdout/stderr RunArgv returns per
	// call — consumed by run_command and the trust gate's per-node
	// deterministic checks (both go through RunArgv). Distinct from git's own
	// dedicated maxGitOutputBytes (internal/tools/git.go), which predates this
	// and stays git-specific.
	MaxOutputBytes int64
	// ExtraPath is appended to the hermetic child PATH (execEnvPath) for
	// RunArgv/RunPipeline children AND git children — the operator's knob for
	// host toolchains living outside the fixed directories (nvm, asdf, custom
	// prefixes). Configured via workspace.exec_path. Empty = the fixed PATH
	// alone, exactly as before.
	ExtraPath []string
	// HomeDir is the $HOME every RunArgv/RunPipeline/git child sees, when set —
	// a per-user directory OUTSIDE any cloned/target repo tree (see
	// workspace.Jail.HomeDir), so a toolchain's own cache/config writes (npm's
	// _cacache, pip's cache, ~/.gitconfig) land there instead of inside a git
	// working tree. Empty falls back to the child's own cwd (the LIVE bug this
	// closes: `npm ci` with HOME=cwd wrote its cache straight into a cloned
	// repo, and git_commit's `add_all` swept up 1,261 cache files alongside 8
	// real ones — see internal/tools/git.go's bulk-commit sanity wall for the
	// other half of this fix). Wired once from internal/serve's buildAgents;
	// nothing here computes it — Caps only carries the resolved value.
	HomeDir string
	// WorkRoot is the calling node's OWN directory (<root>/<user>/<chat>/<node>/) —
	// the writable subtree a sandboxed child gets. It CONTAINS the child's cwd; the
	// cwd alone is not enough, because a private /tmp (see sandbox.go tmpArgs) hides
	// a workspace that lives under /tmp, so anything the child wrote outside the one
	// bound path silently evaporated. Empty ⇒ no node scope: the cwd is bound alone.
	WorkRoot string
	// Sandbox is the OS boundary every RunArgv/RunPipeline child runs inside
	// (SandboxBwrap | SandboxNone — see sandbox.go). The ZERO value is NO
	// boundary, which is why exactly one place in the server resolves it
	// (internal/serve, via workspace.ResolveSandbox — which refuses to start when
	// the configured sandbox isn't usable, and WARNs loudly when it is off).
	// Configured via workspace.sandbox. The jail is a path check on the TOOLS;
	// this is the wall around their children.
	Sandbox SandboxMode
	// Limits are the per-child resource limits (RLIMIT_AS/NPROC/FSIZE — see
	// Limits). Configured via workspace.limits. Zero = inherit the server's.
	Limits Limits
}

// DefaultCaps returns the isolation model's documented defaults.
func DefaultCaps() Caps {
	return Caps{
		MaxReadBytes:   256 * 1024,
		MaxWriteBytes:  2 * 1024 * 1024,
		MaxResults:     200,
		MaxListEntries: 500,
		Timeout:        60 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}
}

// IsZero reports an entirely-unset Caps (callers substitute DefaultCaps).
// Needed because ExtraPath/HomeDir make Caps non-comparable with ==.
func (c Caps) IsZero() bool {
	return c.MaxReadBytes == 0 && c.MaxWriteBytes == 0 && c.MaxResults == 0 &&
		c.MaxListEntries == 0 && c.Timeout == 0 && c.MaxOutputBytes == 0 &&
		len(c.ExtraPath) == 0 && c.HomeDir == "" && c.Sandbox == "" && c.Limits == Limits{}
}
