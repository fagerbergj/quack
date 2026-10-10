package dag

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// readOnlyGateCfg mimics serve's perAgentGateCfg for a read-only agent: ACP-backed,
// read-only, no delivery target.
func readOnlyGateCfg() vetting.Config {
	return vetting.Config{ExternalWorker: true, ReadOnly: true}
}

// A read-only node never gets a deterministic build check: it can never satisfy one,
// and the gate would fail closed on a missing workdir.
func TestNodeGateConfigDropsChecksForReadOnlyNode(t *testing.T) {
	for _, agent := range []string{reviewerAgent, explorerAgent, "web-researcher"} {
		t.Run(agent, func(t *testing.T) {
			plan := Plan{Nodes: []Node{{ID: "n1", AgentName: agent, Checks: []string{"go build ./..."}, Workdir: "review-f65532f/repo"}}}
			cfgFor := func(context.Context, string) vetting.Config { return readOnlyGateCfg() }

			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(restore)

			cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

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

// A writable agent keeps its planner-authored checks and workdir.
func TestNodeGateConfigKeepsChecksForWritableNode(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent, Checks: []string{"go build ./..."}, Workdir: "sub"}}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if len(cfg.Checks) != 1 || cfg.Checks[0] != "go build ./..." {
		t.Errorf("cfg.Checks = %v, want the planner-authored check kept", cfg.Checks)
	}
	if cfg.Workdir != "sub" {
		t.Errorf("cfg.Workdir = %q, want %q kept", cfg.Workdir, "sub")
	}
}

// With Checks nil and DeriveChecks false, checksPassCriterion skips as "not_configured".
func TestNodeGateConfigReviewerHasNoChecksPassCriterion(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent, Checks: []string{"go build ./..."}, Workdir: "guessed"}}}
	cfgFor := func(context.Context, string) vetting.Config { return readOnlyGateCfg() }
	cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if len(cfg.Checks) != 0 {
		t.Fatalf("cfg.Checks = %v, want empty so checks_pass has nothing to run", cfg.Checks)
	}
	if cfg.DeriveChecks {
		t.Fatal("cfg.DeriveChecks = true for a reviewer node, want false")
	}
}
