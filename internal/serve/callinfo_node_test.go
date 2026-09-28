package serve

import (
	"context"
	"reflect"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/functiontool"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestNativeNode_ExtToolSeesPlanCallInfo: a plan's grant and plan-only flag
// travel executor -> gated node -> per-node A2A worker -> extension tool Run ctx.
func TestNativeNode_ExtToolSeesPlanCallInfo(t *testing.T) {
	var mu sync.Mutex
	var got *extsdk.CallInfo
	probe, err := functiontool.New(functiontool.Config{Name: "probe", Description: "records CallInfo"},
		func(ctx adkagent.Context, _ struct{}) (string, error) {
			if ci, ok := extsdk.CallInfoFrom(ctx); ok {
				mu.Lock()
				got = &ci
				mu.Unlock()
			}
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	provider := toolCallProvider(t, "probe", map[string]any{})
	defer provider.Close()
	agent, _ := buildStubNodeAgent(t, provider.URL, []string{"probe"}, []extTool{{provider: "fake", tool: probe}}, artifact.InMemoryService())

	ex := dag.NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"tutor": agent}, nil,
		vetting.NewJudgeFactory(&resumeStubLLM{}, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := dag.Plan{ID: "plan-ci", UserMessage: "x", PlanOnly: true, AllowedDeliveryKinds: []string{"comment"},
		Nodes: []dag.Node{{ID: "n1", AgentName: "tutor", Task: "probe it"}}}
	if _, _, _, err := ex.RunPlanStep(stream.WithTurnID(context.Background(), "turn-7"), plan, "quack", "u1", "chat-1", nil, map[string]bool{"n1": true}); err != nil {
		t.Fatalf("RunPlanStep: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := extsdk.CallInfo{ChatID: "chat-1", UserID: "u1", AllowedDeliveryKinds: []extsdk.DeliveryKind{"comment"}, ReadOnly: true}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("CallInfo = %+v, want %+v", got, want)
	}
}
