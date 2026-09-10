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
