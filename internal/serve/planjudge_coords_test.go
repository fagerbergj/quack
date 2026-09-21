package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workspace"
)

// newPlanJudgeStubProvider serves a fixed OpenAI-compatible chat.completion
// answering submit_plan_verdict(accept:true) - a gate round's non-streaming
// default (RunConfig{}'s StreamingMode) needs no SSE framing.
func newPlanJudgeStubProvider(t *testing.T) config.ProviderConfig {
	t.Helper()
	body := `{"id":"1","object":"chat.completion","model":"judge-model","choices":[{"index":0,"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function",` +
		`"function":{"name":"submit_plan_verdict","arguments":"{\"accept\":true,\"reason\":\"ok\"}"}}]}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return config.ProviderConfig{Kind: "openai", Endpoint: srv.URL, APIKey: "k"}
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
			"judge-test": newPlanJudgeStubProvider(t),
		},
		Gates: config.GatesConfig{
			Rubric: "be good",
			Judge: config.JudgeConfig{
				Provider: "judge-test", Model: "judge-model", MaxRounds: 1,
				Threshold: 0.7, MaxIterations: 2,
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}
	// The ledger is the observation point: a leaked stamp shows up as a
	// non-empty node/agent/round on the emitted llm.call entry, not as an error.
	store := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	var setupFn dag.SetupFunc
	_, _, nodeServers, _, planJudge, _, judgeModel, err := buildAgents(cfg, nil, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, nil, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	// A gated node is mid-round on the gate's judge model.
	judgeModel.(interface{ SetLedgerCoords(ledger.Coords) }).SetLedgerCoords(
		ledger.Coords{ChatID: "other-chat", Node: "n-gated", Agent: "judge", Round: "judge-r1"})

	const chatID = "plan-chat"
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID})
	ok, reason, err := planJudge(ctx, "do a thing", "node a: do the thing", "")
	if err != nil {
		t.Fatalf("plan judge call: %v", err)
	}
	if !ok {
		t.Fatalf("verdict = false (%s), want the fixture's accept", reason)
	}

	entries, err := ledger.ReadObservations(context.Background(), store, chatID)
	if err != nil {
		t.Fatalf("ReadObservations: %v", err)
	}
	var call *ledger.Entry
	for i := range entries {
		if entries[i].Kind == ledger.KindLLMCall {
			call = &entries[i]
			break
		}
	}
	if call == nil {
		t.Fatal("no llm.call entry recorded for the plan-judge round")
	}
	if call.NodeID != "" || call.Agent != "" || call.Round != "" {
		t.Fatalf("llm.call node/agent/round = %q/%q/%q, want all empty (a stamped gate model leaked its node/agent/round into this call)",
			call.NodeID, call.Agent, call.Round)
	}
}
