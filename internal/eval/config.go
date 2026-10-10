// Package eval implements `quack eval`: re-run a recorded bundle against a swapped model and compare judge
// scores. The run loop itself lives in internal/cli.
package eval

import (
	"fmt"
	"sort"

	"github.com/fagerbergj/quack/internal/config"
)

// Roles a model swap supports: the three the per-role env vars name, plus "all".
const (
	RoleCoder      = "coder"
	RoleResearcher = "researcher"
	RoleOrch       = "orch"
	RoleAll        = "all"
)

// OverrideModel swaps role's model in place (judge and media/image agents untouched, so scores compare),
// returning changed names. coder = agents with acp:, researcher = other text agents, orch = orchestrator.
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
