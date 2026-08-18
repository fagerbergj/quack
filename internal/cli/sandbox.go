package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// DefaultSandboxAgent is `quack sandbox`'s default --agent: the reviewer
// whose ACP seat is what most sandbox debugging is chasing.
const DefaultSandboxAgent = "code-reviewer"

// sandboxLocalUserID mirrors internal/serve's localUserID: `quack sandbox`
// mints a jail scope for a synthetic user rather than a real chat, so it
// stays a plain constant here rather than an import from serve (which pulls
// in the whole server wiring for one string).
const sandboxLocalUserID = "local"

// SandboxScratchChat scopes every `quack sandbox` invocation's minted dirs
// under one synthetic "chat" in the jail, node-per-process-per-cwd-choice so
// concurrent invocations never collide.
const SandboxScratchChat = "sandbox-cli"

// NormalizeSandboxMode validates a --mode flag value ("" = use the agent's
// configured sandbox). Kept separate from workspace.ResolveSandbox (which
// also PROBES the mode) so a bad flag value fails fast, before any probing.
func NormalizeSandboxMode(mode string) (workspace.SandboxMode, error) {
	switch workspace.SandboxMode(mode) {
	case "":
		return "", nil
	case workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone:
		return workspace.SandboxMode(mode), nil
	default:
		return "", fmt.Errorf("--mode: unknown sandbox mode %q (want %q, %q, or %q)", mode, workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone)
	}
}

// ResolveSandboxAgent looks up name in cfg.Agents, defaulting to
// DefaultSandboxAgent when name is empty.
func ResolveSandboxAgent(cfg *config.Config, name string) (string, config.AgentConfig, error) {
	if name == "" {
		name = DefaultSandboxAgent
	}
	ac, ok := cfg.Agents[name]
	if !ok {
		return "", config.AgentConfig{}, fmt.Errorf("--agent: %q is not defined in this config's agents:", name)
	}
	return name, ac, nil
}

// SandboxSeat is the resolved jail seat `quack sandbox`'s four subcommands
// all run against: the exact Caps + cwd an ACP agent would get for this
// agent, built the same way internal/serve's buildAgents does (workspaceCaps
// literal + workspace.ResolveSandbox + Jail.HomeDir), so WrapArgv/spawnEnv
// downstream see what the real agent sees. Not itself a copy of any sandbox
// enforcement logic - WrapArgv, ChildPath, SandboxTmpDir all stay in
// internal/workspace and are called, not reimplemented.
type SandboxSeat struct {
	AgentName string
	ReadOnly  bool
	Dir       string // cwd inside the jail
	FreshDir  bool   // true if Dir was minted here (so cleanup removes it)
	Caps      workspace.Caps
}

// ResolveSandboxSeat builds a SandboxSeat for agentName under cfg/jail:
// --cwd "" mints a fresh node-shaped dir under the jail; --cwd "." jails the
// current directory (outside the jail root - WrapArgv/landlockGrants already
// handle a work dir outside caps.WorkRoot, see childArgv's "outside cwd"
// branch); any other --cwd is used as given. --mode overrides the agent's
// configured sandbox.
func ResolveSandboxSeat(cfg *config.Config, jail *workspace.Jail, agentName, cwdFlag, modeFlag string) (SandboxSeat, error) {
	name, ac, err := ResolveSandboxAgent(cfg, agentName)
	if err != nil {
		return SandboxSeat{}, err
	}

	modeOverride, err := NormalizeSandboxMode(modeFlag)
	if err != nil {
		return SandboxSeat{}, err
	}
	wantMode := workspace.SandboxMode(cfg.Workspace.Sandbox)
	if modeOverride != "" {
		wantMode = modeOverride
	}
	mode, err := workspace.ResolveSandbox(wantMode)
	if err != nil {
		return SandboxSeat{}, err
	}

	homeDir, err := jail.HomeDir(sandboxLocalUserID)
	if err != nil {
		return SandboxSeat{}, fmt.Errorf("sandbox: home dir: %w", err)
	}

	dir, fresh, err := resolveSandboxCwd(jail, cwdFlag)
	if err != nil {
		return SandboxSeat{}, err
	}

	scratch, err := jail.ScratchDir(sandboxLocalUserID, SandboxScratchChat, strconv.Itoa(os.Getpid()))
	if err != nil {
		return SandboxSeat{}, fmt.Errorf("sandbox: scratch dir: %w", err)
	}

	readOnly := ac.Acp != nil && ac.Acp.ReadOnly
	caps := workspace.Caps{
		ExtraPath:  cfg.Workspace.ExecPath,
		Env:        cfg.Workspace.Env,
		HomeDir:    homeDir,
		ScratchDir: scratch,
		WorkRoot:   dir,
		Sandbox:    mode,
		Limits: workspace.Limits{
			AddressSpaceMB: cfg.Workspace.Limits.AddressSpaceMB,
			Procs:          cfg.Workspace.Limits.MaxProcs,
			FileSizeMB:     cfg.Workspace.Limits.MaxFileSizeMB,
		},
		ReadOnly: readOnly,
	}

	return SandboxSeat{AgentName: name, ReadOnly: readOnly, Dir: dir, FreshDir: fresh, Caps: caps}, nil
}

// resolveSandboxCwd implements --cwd's three shapes.
func resolveSandboxCwd(jail *workspace.Jail, cwdFlag string) (dir string, fresh bool, err error) {
	switch cwdFlag {
	case "":
		nodeID := "cwd-" + strconv.Itoa(os.Getpid())
		dir, err = jail.EnsureDir(sandboxLocalUserID, SandboxScratchChat, workspace.NodeDir(nodeID))
		if err != nil {
			return "", false, fmt.Errorf("sandbox: mint cwd: %w", err)
		}
		return dir, true, nil
	case ".":
		wd, err := os.Getwd()
		if err != nil {
			return "", false, fmt.Errorf("sandbox: getwd: %w", err)
		}
		return wd, false, nil
	default:
		return cwdFlag, false, nil
	}
}

// Cleanup removes what ResolveSandboxSeat minted for this seat (scratch dir,
// and the cwd if it was freshly minted) - skipped entirely by callers when
// --keep is set.
func (s SandboxSeat) Cleanup() {
	if s.Caps.ScratchDir != "" {
		_ = os.RemoveAll(s.Caps.ScratchDir)
	}
	if s.FreshDir {
		_ = os.RemoveAll(s.Dir)
	}
}

// SandboxPS1 builds the interactive shell's prompt: agent name + ro/rw, per
// the issue comment's "PS1 names the seat unambiguously" requirement.
func SandboxPS1(agentName string, readOnly bool) string {
	rw := "rw"
	if readOnly {
		rw = "ro"
	}
	return fmt.Sprintf("[quack:%s %s] $ ", agentName, rw)
}

// SandboxSpawnEnv mirrors internal/acp.Agent.spawnEnv (PATH/HOME/TMPDIR/
// GIT_*/JAVA_TOOL_OPTIONS via workspace.ChildPath/SandboxTmpDir/
// SandboxJavaToolOptions, all exported seams) plus workspace.env merged with
// the agent's acp.env (mirrors serve.acpChildEnv, unexported there - the
// merge itself is a two-line map fold with no sandbox logic in it, so it's
// duplicated rather than pulled through a new cross-package seam), plus
// extra so a caller can layer PS1/other overrides on top.
func SandboxSpawnEnv(caps workspace.Caps, ac config.AgentConfig, extra map[string]string) []string {
	tmp := workspace.SandboxTmpDir(caps)
	env := []string{
		"PATH=" + workspace.ChildPath(caps),
		"HOME=" + caps.HomeDir,
		"TMPDIR=" + tmp,
		// GOTMPDIR mirrors TMPDIR, same as acp.Agent.spawnEnv (#936/#952): unset,
		// Go's build work dir defaults to os.TempDir(), which the jail doesn't grant.
		"GOTMPDIR=" + tmp,
		"NO_COLOR=1",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_TERMINAL_PROMPT=0",
	}
	if opts := workspace.SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}

	merged := map[string]string{}
	maps.Copy(merged, caps.Env)
	if ac.Acp != nil {
		maps.Copy(merged, ac.Acp.Env)
	}
	maps.Copy(merged, extra)
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		env = append(env, k+"="+merged[k])
	}
	return env
}
