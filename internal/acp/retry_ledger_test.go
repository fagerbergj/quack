package acp_test

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/acp"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// noJudge never gets called: JudgeRounds is 0.
type noJudge struct{}

func (noJudge) Name() string { return "no-judge" }
func (noJudge) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

// TestRetryNode_ACPRoundRecordsAgentInvoke drives a real ACP node through the retry path
// (the one boot resume and `quack chat node retry` share) into the ledger exporter.
func TestRetryNode_ACPRoundRecordsAgentInvoke(t *testing.T) {
	store := ledgertest.NewMemStore()
	restore := otelobs.SetLoggerProviderForTesting(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store)))))
	defer restore()

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coder, err := acp.New("code-implementer", "external coder", acp.Options{
		Command: []string{os.Args[0]}, Env: []string{"QUACK_ACP_FAKE=happy"}, Home: t.TempDir(), Jail: jail, UserID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"code-implementer": coder}, nil,
		vetting.NewJudgeFactory(noJudge{}, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p1", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "code-implementer", Task: "add the feature"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: orchestrator.AppName, UserID: "u1", SessionID: "chat-r",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}

	for _, err := range orch.RetryNode(context.Background(), "u1", "chat-r", "p1", nil, "n1", "") {
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	entries, _ := store.ReadEntries(context.Background(), "chat-r", 0)
	var invokes int
	for _, e := range entries {
		if e.Kind == ledger.KindAgentInvoke && e.NodeID == "n1" {
			invokes++
		}
	}
	if invokes != 1 {
		kinds := make([]string, len(entries))
		for i, e := range entries {
			kinds[i] = e.Kind + "/" + e.NodeID
		}
		t.Fatalf("agent.invoke entries for n1 = %d, want 1; ledger = %v", invokes, kinds)
	}
}
