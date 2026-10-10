package dag

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// agentRoster mirrors the live roster for validators that run before any Planner exists
// (a recordstore kind's init-time Validate closure); NewPlanner keeps it in sync.
var (
	agentRosterMu sync.RWMutex
	agentRoster   []AgentInfo
)

// SetAgentRoster stores the roster AgentNames reports.
func SetAgentRoster(agents []AgentInfo) {
	agentRosterMu.Lock()
	defer agentRosterMu.Unlock()
	agentRoster = agents
}

// AgentNamesFor returns the sorted roster names Executor.Pin pinned (else the current roster), for errors
// that list the model's options.
func AgentNamesFor(ctx context.Context) []string {
	r := pinnedRoster(ctx)
	if r == nil || r.Gen == 0 {
		return agentNameList()
	}
	names := make([]string, len(r.Infos))
	for i, a := range r.Infos {
		names[i] = a.Name
	}
	sort.Strings(names)
	return names
}

func agentNameList() []string {
	agentRosterMu.RLock()
	defer agentRosterMu.RUnlock()
	names := make([]string, len(agentRoster))
	for i, a := range agentRoster {
		names[i] = a.Name
	}
	sort.Strings(names)
	return names
}

// ValidateAgentNameIn rejects an empty name or one outside names (e.g. AgentNamesFor's).
func ValidateAgentNameIn(name string, names []string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent: must not be empty")
	}
	for _, n := range names {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("agent: unknown agent %q; valid agents: %s%s", name, strings.Join(names, ", "), didYouMean(name, names))
}

// ValidateTask rejects a blank task.
func ValidateTask(task string) error {
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("task: must not be empty")
	}
	return nil
}

// checkCommands mirrors workspace.check_commands for the same reason agentRoster exists:
// ValidateChecks runs in an init-time Validate closure that can't reach a live Planner.
var (
	checkCommandsMu sync.RWMutex
	checkCommands   []string
)

// SetCheckCommands stores the prefixes ValidateChecks checks against.
func SetCheckCommands(cmds []string) {
	checkCommandsMu.Lock()
	defer checkCommandsMu.Unlock()
	checkCommands = cmds
}

func checkCommandsSnapshot() []string {
	checkCommandsMu.RLock()
	defer checkCommandsMu.RUnlock()
	return append([]string(nil), checkCommands...)
}

// ValidateChecks applies assemble()'s prefix allowlist using the current Planner's check commands.
func ValidateChecks(checks []string) error {
	return validateChecks(checks, checkCommandsSnapshot())
}

// ValidateWorkdir rejects an absolute or escaping workdir early; empty is fine.
// workspace.Jail.Resolve is the real runtime enforcement.
func ValidateWorkdir(workdir string) error {
	if workdir == "" {
		return nil
	}
	if filepath.IsAbs(workdir) {
		return fmt.Errorf("workdir must be relative, not an absolute path: %q", workdir)
	}
	if cleaned := filepath.Clean(workdir); cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("workdir must stay inside the workspace, not escape it: %q", workdir)
	}
	return nil
}
