package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

func TestNewPlanToolMetadata(t *testing.T) {
	planner := dag.NewPlanner(nil, nil, nil)
	tl, err := NewPlanTool(planner, NewPlanCache(), nil, nil, "", "")
	if err != nil {
		t.Fatalf("NewPlanTool error: %v", err)
	}
	if tl.Name() != "plan" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "plan")
	}
	if !strings.Contains(tl.Description(), "DAG") {
		t.Errorf("Description() = %q, want mention of DAG", tl.Description())
	}
	// Every plan must declare setup + delivery, and the model must never run
	// git/push/PR itself - see github-delivery-architecture.
	for _, want := range []string{"setup", "delivery", "you never run git, push, or open a PR yourself"} {
		if !strings.Contains(tl.Description(), want) {
			t.Errorf("Description() = %q, want it to mention %q", tl.Description(), want)
		}
	}
}

// summarizePlan is the summary the model sees back after calling plan - it
// must surface the declared setup/delivery so the model can catch its own
// mistake before calling execute.
func TestSummarizePlanIncludesSetupAndDelivery(t *testing.T) {
	plan := &dag.Plan{
		Nodes:    []dag.Node{{ID: "impl", AgentName: "code-implementer"}},
		Setup:    &dag.Setup{BaseRef: "main", WorkBranch: "feat/widget"},
		Delivery: &dag.Delivery{Kind: "pull_request"},
	}
	got := summarizePlan(plan)
	for _, want := range []string{"feat/widget", "pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizePlan = %q, want it to contain %q", got, want)
		}
	}
}

// recordCapture is a minimal sdklog.Exporter for direct record inspection.
type recordCapture struct{ records []sdklog.Record }

func (c *recordCapture) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *recordCapture) Shutdown(context.Context) error   { return nil }
func (c *recordCapture) ForceFlush(context.Context) error { return nil }

func TestEmitPlanEvent_ProducesWellFormedEvent(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	plan := &dag.Plan{ID: "plan-123", Nodes: []dag.Node{{ID: "impl", AgentName: "code-implementer"}}}
	emitPlanEvent(newFakeCtx(), plan)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]otellog.Value{}
	capExp.records[0].WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs["gen_ai.operation.name"].AsString(); got != "plan" {
		t.Errorf("gen_ai.operation.name = %q, want plan", got)
	}
	if got := attrs["gen_ai.workflow.name"].AsString(); got != "plan-123" {
		t.Errorf("gen_ai.workflow.name = %q, want plan-123", got)
	}
	if got := attrs["gen_ai.conversation.id"].AsString(); got != "sess" {
		t.Errorf("gen_ai.conversation.id = %q, want sess (from tc.SessionID())", got)
	}
	if attrs["gen_ai.output.messages"].AsString() == "" {
		t.Error("gen_ai.output.messages missing the marshaled plan")
	}
}

// TestPlanEdges verifies the wire edge list is derived from each node's
// DependsOn (From=dep, To=node), and a no-dependency node contributes nothing.
func TestPlanEdges(t *testing.T) {
	nodes := []dag.Node{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a", "b"}},
	}
	got := planEdges(nodes)
	want := []stream.DagEdgeDef{
		{From: "a", To: "b"},
		{From: "a", To: "c"},
		{From: "b", To: "c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planEdges = %+v, want %+v", got, want)
	}
}
