// Package eval implements `quack eval` (#606): re-run a recorded bundle's
// user turns live against a swapped model, and compare the resulting judge scores to the recording's own. It owns the config-level model swap
// (config.go) and the recorded-vs-new comparison (compare.go); the run itself (a fresh chat, turn-by-turn) is driven by internal/cli, which already owns every other non-interactive CLI run loop.
package eval

import (
	"fmt"
	"sort"

	"github.com/fagerbergj/quack/internal/config"
)

// Roles v1 supports for a model swap: the same three the codebase's own
// per-role env vars name (QUACK_CODER_MODEL / QUACK_RESEARCHER_MODEL /
// QUACK_ORCH_MODEL - see AGENTS.md), plus "all".
const (
	RoleCoder      = "coder"
	RoleResearcher = "researcher"
	RoleOrch       = "orch"
	RoleAll        = "all"
)

// OverrideModel mutates cfg IN PLACE, swapping the model bound to every agent
// in role (or every agent for RoleAll) to model, returning the changed names.
// It never touches gates.judge.model/provider - an eval compares a swapped
// WORKER against the SAME judge, so the judge must stay fixed for the two
// runs' scores to mean anything - and it never touches a media/image agent (out of scope).
//
// Role membership is structural, not name-listed: "coder" = every agents:
// entry with an acp: block set (all code agents run external over ACP; its
// model binds through the generated PI_ACP_CONFIG, so overriding
// AgentConfig.Model is the whole fix); "researcher" = every OTHER text-only agent (excludes media/image readers); "orch" = the orchestrator's own top-level model (OrchestratorConfig.Model).
func OverrideModel(cfg *config.Config, role, model string) ([]string, error) {
	switch role {
	case RoleCoder, RoleResearcher, RoleOrch, RoleAll:
	default:
		return nil, fmt.Errorf("eval: unknown --role %q (want coder, researcher, orch, or all)", role)
	}
	if model == "" {
		return nil, fmt.Errorf("eval: --model is required")
	}

	var changed []string
	if role == RoleOrch || role == RoleAll {
		cfg.Orchestrator.Model = model
		changed = append(changed, "orchestrator")
	}
	for name, ac := range cfg.Agents {
		isCoder := ac.Acp != nil
		isResearcher := !isCoder && len(ac.Inputs) == 0
		match := (role == RoleCoder && isCoder) ||
			(role == RoleResearcher && isResearcher) ||
			(role == RoleAll && (isCoder || isResearcher))
		if !match {
			continue
		}
		ac.Model = model
		cfg.Agents[name] = ac
		changed = append(changed, name)
	}
	sort.Strings(changed)
	return changed, nil
}
