package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/acp"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// DefaultSandboxAgent is `quack sandbox`'s default --agent: the reviewer
// whose ACP seat is what most sandbox debugging is chasing.
const DefaultSandboxAgent = "code-reviewer"

// sandboxLocalUserID mirrors serve's localUserID as a literal; importing serve would pull in the
// whole server wiring for one string.
const sandboxLocalUserID = "local"

// SandboxScratchChat scopes every `quack sandbox` invocation's dirs under one synthetic chat in the jail.
const SandboxScratchChat = "sandbox-cli"

// normalizeSandboxMode validates --mode ("" = the agent's configured sandbox) before
// workspace.ResolveSandbox probes anything, so a bad value fails fast.
func normalizeSandboxMode(mode string) (workspace.SandboxMode, error) {
	switch workspace.SandboxMode(mode) {
	case "":
		return "", nil
	case workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone:
		return workspace.SandboxMode(mode), nil
	default:
		return "", fmt.Errorf("--mode: unknown sandbox mode %q (want %q, %q, or %q)", mode, workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone)
	}
}

// SandboxAgentNames returns cfg's agent names, sorted - shared by
// ResolveSandboxAgent's error text and `--agent`'s shell completion.
func SandboxAgentNames(cfg *config.Config) []string {
	return slices.Sorted(maps.Keys(cfg.Agents))
}

// ResolveSandboxAgent looks up name in cfg.Agents, defaulting to
// DefaultSandboxAgent when name is empty.
func ResolveSandboxAgent(cfg *config.Config, name string) (string, config.AgentConfig, error) {
	if name == "" {
		name = DefaultSandboxAgent
	}
	ac, ok := cfg.Agents[name]
	if !ok {
		names := SandboxAgentNames(cfg)
		if len(names) == 0 {
			return "", config.AgentConfig{}, fmt.Errorf("--agent: %q is not defined in this config's agents: (none configured)", name)
		}
		return "", config.AgentConfig{}, fmt.Errorf("--agent: %q is not defined in this config's agents: %s", name, strings.Join(names, ", "))
	}
	return name, ac, nil
}

// SandboxSeat is the jail seat `quack sandbox` runs against: the Caps and cwd an ACP agent would get,
// built as serve's buildAgents does. Enforcement itself stays in internal/workspace.
type SandboxSeat struct {
	AgentName string
	ReadOnly  bool
	Dir       string // cwd inside the jail
	FreshDir  bool   // true if Dir was minted here (so cleanup removes it)
	Caps      workspace.Caps
}

// ResolveSandboxSeat builds agentName's seat: --cwd "" mints a fresh dir under the jail, "." jails the
// current directory, anything else is used as given. --mode overrides the agent's sandbox.
func ResolveSandboxSeat(cfg *config.Config, jail *workspace.Jail, agentName, cwdFlag, modeFlag string) (SandboxSeat, error) {
	name, ac, err := ResolveSandboxAgent(cfg, agentName)
	if err != nil {
		return SandboxSeat{}, err
	}

	modeOverride, err := normalizeSandboxMode(modeFlag)
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
		ReadOnly:  readOnly,
		BuildDirs: cfg.Workspace.BuildDirs,
	}
	// Pre-create before the mode confines dir, as tools.SetupWorktree/SetupClone do;
	// a no-op when the dirs already exist.
	workspace.PrecreateBuildDirs(dir, caps.BuildDirs)

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

// Cleanup removes what ResolveSandboxSeat minted (scratch dir, and a freshly minted cwd).
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

// SandboxSpawnEnv is the ACP child's env via acp.SpawnEnv, the same function Agent.spawnEnv uses,
// plus extra so a caller can layer overrides such as PS1.
func SandboxSpawnEnv(caps workspace.Caps, ac config.AgentConfig, extra map[string]string) []string {
	// The agent's opts.Env is workspace.env merged with its acp.env, in the
	// same order serve builds it - hand that to the ONE real builder.
	merged := map[string]string{}
	maps.Copy(merged, caps.Env)
	if ac.Acp != nil {
		maps.Copy(merged, ac.Acp.Env)
	}
	var agentEnv []string
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		agentEnv = append(agentEnv, k+"="+merged[k])
	}
	env := acp.SpawnEnv(caps.HomeDir, agentEnv, caps)
	// extra (PS1 etc.) layers LAST so it wins on duplicate keys.
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}
	return env
}
