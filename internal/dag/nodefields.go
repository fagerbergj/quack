package dag

import (
	"fmt"
	"path/filepath"
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

// AgentNames returns the current agent roster's names, sorted - for a
// validation error that needs to show the model its options (e.g. "give
// node_id ... or agent: one of <AgentNames>").
func AgentNames() []string { return agentNameList() }

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

// ValidateSetupOverride rejects a model-submitted setup that disagrees with
// the trigger's own setup on repo or base_ref. trigger's copy came from the
// real PR/issue data and always overrides whatever the model submits at
// execute time regardless (githubSetup) - so a mismatched override is either
// the model hallucinating which repo it thinks it's working in, or a stale
// assumption, and catching it here costs one cheap validation, not a
// plan-judge round. trigger nil (no trigger-supplied setup this dispatch) or
// submitted nil (nothing to check) are both no-ops.
func ValidateSetupOverride(submitted, trigger *Setup) error {
	if submitted == nil || trigger == nil {
		return nil
	}
	if submitted.Repo != "" && submitted.Repo != trigger.Repo {
		return fmt.Errorf("setup.repo must be %q; the run can only clone the trigger's own repo", trigger.Repo)
	}
	if submitted.BaseRef != "" && submitted.BaseRef != trigger.BaseRef {
		return fmt.Errorf("setup.base_ref must be %q; the trigger's base branch is not negotiable", trigger.BaseRef)
	}
	return nil
}

// ValidateWorkdir rejects an assignment's workdir that isn't a plain relative
// path inside the workspace - an early, cheap syntactic check at
// plan-authoring time; internal/workspace.Jail.Resolve is the actual runtime
// enforcement once a node's clone exists, this just catches the obviously
// wrong case before a node ever runs on it. Empty is fine (workdir is optional).
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
