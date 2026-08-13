package memory

import (
	"context"
	"iter"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// fakeModel is a consolidator stub that replies with a fixed text (canned JSON ops).
// usage is nil by default (existing callers get no usage metadata, matching prior behaviour).
type fakeModel struct {
	reply string
	usage *genai.GenerateContentResponseUsageMetadata
}

func (fakeModel) Name() string { return "fake-consolidator" }

func (f fakeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: f.reply}}}, UsageMetadata: f.usage}, nil)
	}
}

func TestCommit_AddThenRecall(t *testing.T) {
	ctx := context.Background()
	consolidator := fakeModel{reply: "```json\n{\"ops\":[{\"action\":\"ADD\",\"content\":\"transportforireland.ie is authoritative for Irish transit\",\"kind\":\"source\"}]}\n```"}
	s := newSQLiteStore(t, "task", consolidator)

	sc := Scope{Role: RoleResearch, User: "u1", Legacy: "web-researcher"}
	n, err := s.Commit(ctx, sc, "web-researcher", Provenance{},
		[]Candidate{{Content: "use the official transit site", Metadata: map[string]string{"kind": "source"}}},
		"Dublin buses run by transportforireland.ie ...")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("Commit wrote %d, want 1", n)
	}

	// Recall goes through the writer's own view (its role + user buckets).
	resp, err := s.View(sc, nil).SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit"})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall got %d, want 1 committed memory", len(resp.Memories))
	}
}

// TestCommit_Noop verifies the gate's vetting drop: when the consolidator returns
// no ops (nothing worth keeping), nothing is written.
func TestCommit_Noop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[]}`})

	n, err := s.Commit(ctx, Scope{Role: RoleResearch, User: "u1"}, "web-researcher", Provenance{}, []Candidate{{Content: "today's bus fare is 2 euro"}}, "")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 0 {
		t.Fatalf("Commit wrote %d, want 0 (vetting should have dropped it)", n)
	}
}

// TestCommit_NoConsolidator guards the read-only-store error path (no LLM call).
func TestCommit_NoConsolidator(t *testing.T) {
	s := &Store{} // no consolidator
	if _, err := s.Commit(context.Background(), Scope{User: "u1"}, "a", Provenance{}, []Candidate{{Content: "x"}}, ""); err == nil {
		t.Fatal("Commit with nil consolidator should error")
	}
}

// TestCommit_ConsolidatorDefaultAgentFillsTokenUsage pins serve.go's openMemory
// wiring: Commit runs from a background goroutine or a tool call whose ctx never
// carries the node's coords, so the consolidator's tracedModel needs the
// SetDefaultAgent("memory") fallback to attribute its token usage at all.
func TestCommit_ConsolidatorDefaultAgentFillsTokenUsage(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}

	reply := fakeModel{
		reply: `{"ops":[{"action":"ADD","content":"fact","kind":"source"}]}`,
		usage: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
	}
	consolidator := inference.TracedModelForTesting(reply, "consolidator-test-model")
	if da, ok := consolidator.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent("memory")
	} else {
		t.Fatal("TracedModelForTesting result does not implement SetDefaultAgent")
	}
	s := newSQLiteStore(t, "task", consolidator)

	// No ledger coords on ctx - mirrors Commit's real callers (background goroutine, tool call).
	if _, err := s.Commit(context.Background(), Scope{Role: RoleResearch, User: "u1"},
		"web-researcher", Provenance{}, []Candidate{{Content: "use the official transit site"}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "gen_ai.client.token.usage" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
			}
			for _, dp := range sum.DataPoints {
				agentVal, _ := dp.Attributes.Value(attribute.Key("agent"))
				if agentVal.AsString() == "memory" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no gen_ai.client.token.usage data point carries agent=memory - the consolidator's SetDefaultAgent fallback never reached the metric")
	}
}

func TestNeighbourProbe(t *testing.T) {
	// Short input passes through untouched.
	if got := neighbourProbe("hello", nil); got != "hello" {
		t.Fatalf("short probe = %q, want hello", got)
	}

	// A long source answer is capped to maxProbeRunes.
	long := strings.Repeat("x", maxProbeRunes*3)
	if got := neighbourProbe(long, nil); len([]rune(got)) != maxProbeRunes {
		t.Fatalf("probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}

	// Staged content leads, so it survives truncation even with a huge answer.
	got := neighbourProbe(strings.Repeat("y", maxProbeRunes*3), []Candidate{{Content: "STAGED-FACT"}})
	if !strings.HasPrefix(got, "STAGED-FACT") {
		t.Fatalf("probe must lead with staged content, got %q...", got[:20])
	}
	if len([]rune(got)) != maxProbeRunes {
		t.Fatalf("capped probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}
}
