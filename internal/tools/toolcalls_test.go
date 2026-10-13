package tools

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	adkagent "google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

type refusingRunnable struct{ fakeRunnable }

func (*refusingRunnable) Run(adkagent.Context, any) (map[string]any, error) {
	return nil, errors.New("write_review: schema refused the record")
}

// Every call counts, even under policy 0 (artifact tools emit no ledger row), billed through the stamped
// coords like a model call; an error is a rejection the model spends a request recovering from.
func TestToolCalls_CountRejectionsForTheNodeAgent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}
	h := NewHooks(Deps{}, 0)
	h.SetLedgerCoords(ledger.Coords{ChatID: "chat-1", Node: "n1", Agent: ledger.JudgeAgent, JudgedAgent: "code-reviewer"})

	if _, err := hook(h, 0, &fakeRunnable{}).Run(newFakeCtx(), map[string]any{}); err != nil {
		t.Fatalf("Run ok tool: %v", err)
	}
	if _, err := hook(h, 0, &refusingRunnable{}).Run(newFakeCtx(), map[string]any{}); err == nil {
		t.Fatal("Run refusing tool: want its error")
	}

	ps, err := otelobs.PointsForTesting(reader, "quack.tool.calls")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, p := range ps {
		if p.Attrs[otelobs.GenAIAgentName] != "code-reviewer" || p.Attrs["role"] != otelobs.RoleJudge || p.Attrs["tool"] != "risky_op" {
			t.Errorf("quack.tool.calls point %v, want agent code-reviewer, role judge, tool risky_op", p.Attrs)
		}
		got[p.Attrs["rejected"]] += p.Value
	}
	if got["true"] != 1 || got["false"] != 1 {
		t.Errorf("quack.tool.calls by rejected = %v, want one each", got)
	}
}
