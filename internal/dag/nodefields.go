package dag

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// agentRoster mirrors the live agent roster outside any single Planner
// instance, so a validator that must exist before a Planner is constructed
// (e.g. a recordstore kind's init-time Validate closure) can still check a
// node's agent field - SetAgentRoster is called once by NewPlanner, keeping
// this in sync with whatever roster the Planner itself was built with.
var (
	agentRosterMu sync.RWMutex
	agentRoster   []AgentInfo
)

// SetAgentRoster stores the roster ValidateAgentName checks names against.
func SetAgentRoster(agents []AgentInfo) {
	agentRosterMu.Lock()
	defer agentRosterMu.Unlock()
	agentRoster = agents
}

// AgentInfoFor returns the current roster's entry for name, ok=false if
// unknown - the display lookup (context window, default artifact kind)
// create_plan/edit_plan use to build the dag_plan SSE event before execute
// (and its own Build call) resolves the same values authoritatively.
func AgentInfoFor(name string) (AgentInfo, bool) {
	agentRosterMu.RLock()
	defer agentRosterMu.RUnlock()
	for _, a := range agentRoster {
		if a.Name == name {
			return a, true
		}
	}
	return AgentInfo{}, false
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

// ValidateAgentName rejects a name outside the current agent roster - the
// shape-independent half of "unknown agent" validation (a node's `agent`
// field), reusable by any node-shaped record kind regardless of how its
// nodes are stored (inline, or as their own first-class records).
func ValidateAgentName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent: must not be empty")
	}
	names := agentNameList()
	for _, n := range names {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("agent: unknown agent %q; valid agents: %s%s", name, strings.Join(names, ", "), didYouMean(name, names))
}

// ValidateTask rejects an empty task string - every node-shaped record kind
// requires one self-contained, non-empty task regardless of how its nodes
// are stored.
func ValidateTask(task string) error {
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("task: must not be empty")
	}
	return nil
}

// checkCommands mirrors the live workspace.check_commands prefix allowlist,
// the same reason agentRoster exists: ValidateChecks runs inside a
// recordstore kind's init-time Validate closure, which has no way to reach
// a live Planner instance.
var (
	checkCommandsMu sync.RWMutex
	checkCommands   []string
)

// SetCheckCommands stores the prefixes ValidateChecks checks a node's
// `checks` field against.
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

// ValidateChecks is the shape-independent half of "checks" validation -
// reuses the same prefix-allowlist validateChecks already enforces in
// assemble(), against whatever check commands the current Planner was built
// with.
func ValidateChecks(checks []string) error {
	return validateChecks(checks, checkCommandsSnapshot())
}
