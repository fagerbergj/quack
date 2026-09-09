package dag

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// readOnlyGateCfg mimics a real code-reviewer/code-explorer's startup-time
// config (internal/serve/serve.go's perAgentGateCfg): ACP-backed, read-only,
// no delivery target - the shape any read-only agent's cfgFor hands back,
// regardless of what a planner-authored node asks for.
func readOnlyGateCfg() vetting.Config {
	return vetting.Config{ExternalWorker: true, ReadOnly: true}
}

// TestNodeGateConfigDropsChecksForReadOnlyNode pins the prod failure (trust
// gate fail-closed: `checks_pass: check "go build ./...": workspace: workdir
// ".../review-f65532f/repo" does not exist`) at its root: a read-only node
// (any agent lacking write/push tools, not just code-reviewer by name) never
// gets a deterministic build check, because it can never satisfy one.
func TestNodeGateConfigDropsChecksForReadOnlyNode(t *testing.T) {
	for _, agent := range []string{reviewerAgent, explorerAgent, "web-researcher"} {
		t.Run(agent, func(t *testing.T) {
			plan := Plan{Nodes: []Node{{ID: "n1", AgentName: agent, Checks: []string{"go build ./..."}, Workdir: "review-f65532f/repo"}}}
			cfgFor := func(string) vetting.Config { return readOnlyGateCfg() }

			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(restore)

			cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

			if cfg.Checks != nil {
				t.Errorf("cfg.Checks = %v, want nil for a read-only node", cfg.Checks)
			}
			if cfg.Workdir != "" {
				t.Errorf("cfg.Workdir = %q, want empty for a read-only node", cfg.Workdir)
			}
			out := buf.String()
			if !strings.Contains(out, "level=INFO") {
				t.Errorf("expected an INFO log dropping the checks, got: %s", out)
			}
			if !strings.Contains(out, "n1") || !strings.Contains(out, agent) {
				t.Errorf("expected the log to name the node and agent, got: %s", out)
			}
		})
	}
}

// TestNodeGateConfigKeepsChecksForWritableNode is the contrast case: an
// implementer (or any writable agent) keeps its planner-authored checks and
// workdir untouched.
func TestNodeGateConfigKeepsChecksForWritableNode(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent, Checks: []string{"go build ./..."}, Workdir: "sub"}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if len(cfg.Checks) != 1 || cfg.Checks[0] != "go build ./..." {
		t.Errorf("cfg.Checks = %v, want the planner-authored check kept", cfg.Checks)
	}
	if cfg.Workdir != "sub" {
		t.Errorf("cfg.Workdir = %q, want %q kept", cfg.Workdir, "sub")
	}
}

// TestNodeGateConfigReviewerHasNoChecksPassCriterion proves the practical
// consequence for a reviewer node: with Checks nil and DeriveChecks false
// (reviewer is never the implementer), vetting.checksPassCriterion's own
// "not_configured" skip fires - the gate never evaluates a checks_pass
// criterion for it at all.
func TestNodeGateConfigReviewerHasNoChecksPassCriterion(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent, Checks: []string{"go build ./..."}, Workdir: "guessed"}}}
	cfgFor := func(string) vetting.Config { return readOnlyGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if len(cfg.Checks) != 0 {
		t.Fatalf("cfg.Checks = %v, want empty so checks_pass has nothing to run", cfg.Checks)
	}
	if cfg.DeriveChecks {
		t.Fatal("cfg.DeriveChecks = true for a reviewer node, want false")
	}
}
