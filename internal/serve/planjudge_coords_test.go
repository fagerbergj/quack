package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Replay stream keys are Node/Agent/Round, so a plan-judge call that inherits a
// gated node's stamp resolves a stream the bundle doesn't have -> MissError.
func writePlanJudgeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	out := `{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"submit_plan_verdict\",\"args\":{\"accept\":true,\"reason\":\"ok\"}}}]}`
	line := `{"timestamp":"2026-01-01T00:00:00Z","attributes":{` +
		`"gen_ai.operation.name":"chat","gen_ai.request.model":"judge-model",` +
		`"gen_ai.response.model":"judge-model","gen_ai.output.messages":"` + out + `"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildAgents_PlanJudgeDoesNotInheritGatedNodeStamp(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	builtinSkillSrc := newSkillSource(nil)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		t.Fatal(err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"replay-test": {Kind: "replay", Bundle: writePlanJudgeFixture(t)},
		},
		Gates: config.GatesConfig{
			Rubric: "be good",
			Judge: config.JudgeConfig{
				Provider: "replay-test", Model: "judge-model", MaxRounds: 1,
				Threshold: 0.7, MaxIterations: 2,
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}
	var setupFn dag.SetupFunc
	_, _, nodeServers, _, planJudge, _, judgeModel, err := buildAgents(cfg, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, nil, nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	// A gated node is mid-round on the gate's judge model.
	judgeModel.(interface{ SetLedgerCoords(ledger.Coords) }).SetLedgerCoords(
		ledger.Coords{ChatID: "other-chat", Node: "n-gated", Agent: "judge", Round: "judge-r1"})

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "plan-chat"})
	ok, reason, err := planJudge(ctx, "do a thing", "node a: do the thing")
	if err != nil {
		t.Fatalf("plan judge call: %v (a stamped gate model leaked its node/agent/round into this call)", err)
	}
	if !ok {
		t.Fatalf("verdict = false (%s), want the fixture's accept", reason)
	}
}
