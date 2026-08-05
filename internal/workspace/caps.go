package workspace

import "time"

// Caps bounds every workspace tool call. A cap is never a hard failure for a
// read-shaped result (read_file/list_dir/glob/grep all carry a `truncated`
// bool and truncate loudly instead) - see each tool's doc comment for exactly
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
	// call - consumed by run_command and the trust gate's per-node
	// deterministic checks (both go through RunArgv). Distinct from git's own
	// dedicated maxGitOutputBytes (internal/tools/git.go), which predates this
	// and stays git-specific.
	MaxOutputBytes int64
	// ExtraPath is appended to the hermetic child PATH (execEnvPath) for
	// RunArgv/RunPipeline children AND git children - the operator's knob for
	// host toolchains living outside the fixed directories (nvm, asdf, custom
	// prefixes). Configured via workspace.exec_path. Empty = the fixed PATH
	// alone, exactly as before.
	ExtraPath []string
	// Env is extra environment handed to every RunArgv/RunPipeline child, on
	// top of the fixed PATH/HOME - the operator's knob for a toolchain that
	// must be FOUND, not just be on PATH (JAVA_HOME, ANDROID_HOME, a GOROOT
	// outside /usr). Configured via workspace.env; config validation already
	// rejects PATH/HOME keys, so this never fights childPath/childHome.
	Env map[string]string
	// HomeDir is the $HOME every RunArgv/RunPipeline/git child sees, when set -
	// kept OUTSIDE any cloned/target repo tree so toolchain cache/config writes
	// (npm's _cacache, pip's cache, ~/.gitconfig) can't land inside a git working
	// tree and get swept into a commit. Empty falls back to the child's own cwd.
	HomeDir string
	// WorkRoot is the calling node's OWN directory, mounted inside the sandbox
	// namespace as the fixed SandboxWorkRoot. It must CONTAIN the child's cwd,
	// not just equal it - a private /tmp (see tmpArgs) hides writes under /tmp
	// otherwise. It is also the fs tools' "/" (internal/tools/cwd.go); any caller
	// whose output the model reads (run_command, gate checks) must set it.
	WorkRoot string
	// Sandbox is the OS boundary every RunArgv/RunPipeline child runs inside
	// (SandboxBwrap | SandboxNone - see sandbox.go). The ZERO value is NO
	// boundary, which is why exactly one place in the server resolves it
	// (internal/serve, via workspace.ResolveSandbox - which refuses to start when
	// the configured sandbox isn't usable, and WARNs loudly when it is off).
	// Configured via workspace.sandbox. The jail is a path check on the TOOLS;
	// this is the wall around their children.
	Sandbox SandboxMode
	// Limits are the per-child resource limits (RLIMIT_AS/NPROC/FSIZE - see
	// Limits). Configured via workspace.limits. Zero = inherit the server's.
	Limits Limits
	// ExtraRO grants additional read-only directories to every sandboxed
	// RunArgv/RunPipeline child, on top of ExtraPath's PATH-scoped toolchain
	// binds - e.g. a GitHub context directory (#660) that sits OUTSIDE the
	// node's own WorkRoot (a sibling of the clone, not a subdirectory) and so
	// needs its own grant. Absolute host paths; SandboxNone ignores it (no
	// boundary to grant against).
	ExtraRO []string
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
// Needed because ExtraPath/Env/HomeDir make Caps non-comparable with ==.
func (c Caps) IsZero() bool {
	return c.MaxReadBytes == 0 && c.MaxWriteBytes == 0 && c.MaxResults == 0 &&
		c.MaxListEntries == 0 && c.Timeout == 0 && c.MaxOutputBytes == 0 &&
		len(c.ExtraPath) == 0 && len(c.Env) == 0 && c.HomeDir == "" && c.Sandbox == "" && c.Limits == Limits{} &&
		len(c.ExtraRO) == 0
}
