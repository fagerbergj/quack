package acp

import (
	"context"
	"iter"
	"math"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// stubNativeModel is a minimal model.LLM for TestNoDoubleCounting - just
// enough for inference.TracedModelForTesting to wrap and report usage.
type stubNativeModel struct{ name string }

func (s *stubNativeModel) Name() string { return s.name }

func (s *stubNativeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{Text: "answer"}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     200,
				CandidatesTokenCount: 60,
			},
		}, nil)
	}
}

// newUsageTestMeter installs a fresh manual-reader meter as the otelobs singleton.
func newUsageTestMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}
	return reader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name == name {
				return met, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func tokenUsagePoints(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.DataPoint[int64] {
	t.Helper()
	met, ok := collectMetric(t, reader, "gen_ai.client.token.usage")
	if !ok {
		return nil
	}
	sum, ok := met.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
	}
	out := map[string]metricdata.DataPoint[int64]{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key(otelobs.GenAITokenType))
		out[v.AsString()] = dp
	}
	return out
}

func costPoint(t *testing.T, reader *sdkmetric.ManualReader) (metricdata.DataPoint[float64], bool) {
	t.Helper()
	met, ok := collectMetric(t, reader, "gen_ai.client.cost")
	if !ok {
		return metricdata.DataPoint[float64]{}, false
	}
	sum, ok := met.Data.(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) == 0 {
		return metricdata.DataPoint[float64]{}, false
	}
	return sum.DataPoints[0], true
}

func attrVal(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

// usageTestAgent builds an Agent with the metrics-relevant Options serve.go passes.
func usageTestAgent(t *testing.T, mode string, pricing *config.ModelPricing) *Agent {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:   []string{os.Args[0]},
		Env:       []string{"QUACK_ACP_FAKE=" + mode},
		Home:      t.TempDir(),
		Jail:      jail,
		UserID:    "u1",
		ModelName: "qwen3-coder",
		Pricing:   pricing,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestRound_UsageEmitsOncePerRound: several streamed updates precede the PromptResponse,
// but the metric lands exactly once, from PromptResponse.Usage.
func TestRound_UsageEmitsOncePerRound(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage", nil)
	a.SetLedgerCoords(ledger.Coords{Agent: "code-implementer", User: "local", Source: "github"})

	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true })
	if err != nil {
		t.Fatalf("round: %v", err)
	}

	points := tokenUsagePoints(t, reader)
	want := map[string]int64{
		otelobs.GenAITokenTypeInput:     100,
		otelobs.GenAITokenTypeOutput:    50,
		otelobs.GenAITokenTypeReasoning: 10,
		otelobs.GenAITokenTypeCached:    25,
	}
	for typ, wantN := range want {
		dp, ok := points[typ]
		if !ok {
			t.Fatalf("no data point for token_type=%q (got %v)", typ, points)
		}
		if dp.Value != wantN {
			t.Errorf("token_type=%q value = %d, want %d (double-emission would show a multiple)", typ, dp.Value, wantN)
		}
		if got := attrVal(dp.Attributes, otelobs.GenAIRequestModel); got != "qwen3-coder" {
			t.Errorf("token_type=%q model = %q, want qwen3-coder", typ, got)
		}
		if got := attrVal(dp.Attributes, "agent"); got != "code-implementer" {
			t.Errorf("token_type=%q agent = %q, want code-implementer", typ, got)
		}
		if got := attrVal(dp.Attributes, "user"); got != "local" {
			t.Errorf("token_type=%q user = %q, want local", typ, got)
		}
		if got := attrVal(dp.Attributes, "source"); got != "github" {
			t.Errorf("token_type=%q source = %q, want github", typ, got)
		}
	}
}

// TestRound_UsageAbsent_NoMetrics: an agent without the usage capability produces no data point, not a zero.
func TestRound_UsageAbsent_NoMetrics(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage-none", nil)
	a.SetLedgerCoords(ledger.Coords{Agent: "code-implementer"})

	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	if _, ok := collectMetric(t, reader, "gen_ai.client.token.usage"); ok {
		t.Error("gen_ai.client.token.usage recorded for a round with no reported usage")
	}
	if _, ok := costPoint(t, reader); ok {
		t.Error("gen_ai.client.cost recorded for a round with no reported usage")
	}
}

// TestRound_Cost_ComputedFromConfiguredPricing: cached reads bill at the input rate,
// matching inference.recordUsageMetrics.
func TestRound_Cost_ComputedFromConfiguredPricing(t *testing.T) {
	reader := newUsageTestMeter(t)
	pricing := &config.ModelPricing{InputPerMTok: 2, OutputPerMTok: 4}
	a := usageTestAgent(t, "usage", pricing)
	a.SetLedgerCoords(ledger.Coords{Agent: "code-implementer"})

	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	dp, ok := costPoint(t, reader)
	if !ok {
		t.Fatal("gen_ai.client.cost was never recorded")
	}
	// (100 input + 25 cached) * $2/Mtok + (50 output + 10 reasoning) * $4/Mtok
	// = 0.00025 + 0.00024 = 0.00049
	const want = 0.00049
	if diff := dp.Value - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("gen_ai.client.cost = %v, want %v", dp.Value, want)
	}
}

// TestRound_NoPricing_NoCostMetric guards "never guess a price".
func TestRound_NoPricing_NoCostMetric(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage", nil)
	a.SetLedgerCoords(ledger.Coords{Agent: "code-implementer"})

	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	if _, ok := costPoint(t, reader); ok {
		t.Error("gen_ai.client.cost recorded with no configured pricing")
	}
}

// TestRound_NoCoords_AttributionOmitted guards "omit, don't guess": a round
// with no SetLedgerCoords stamp must not emit empty-string attributes.
func TestRound_NoCoords_AttributionOmitted(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage", nil)

	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	points := tokenUsagePoints(t, reader)
	dp, ok := points[otelobs.GenAITokenTypeInput]
	if !ok {
		t.Fatal("no data point for token_type=input")
	}
	for _, key := range []string{"user", "source"} {
		if _, ok := dp.Attributes.Value(attribute.Key(key)); ok {
			t.Errorf("attribute %q set with no coords stamped, want it omitted", key)
		}
	}
	// The agent is known without coords: it is this agent, as its llm.call rows say.
	if got := attrVal(dp.Attributes, "agent"); got != a.name {
		t.Errorf("agent = %q, want %q", got, a.name)
	}
}

// TestNoDoubleCounting_NativeAndACPEmitIndependently: native calls emit via tracedModel and ACP rounds via
// this package; each path's numbers show up exactly once against the same reader.
func TestNoDoubleCounting_NativeAndACPEmitIndependently(t *testing.T) {
	reader := newUsageTestMeter(t)

	// Native side: tracedModel.GenerateContent, the ONLY native emission path.
	native := inference.TracedModelForTesting(&stubNativeModel{name: "native-model"}, "native-model")
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "web-researcher", User: "local", Source: "app"})
	for range native.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	// ACP side: this package's round() seam.
	a := usageTestAgent(t, "usage", nil)
	a.SetLedgerCoords(ledger.Coords{Agent: "code-implementer", User: "local", Source: "github"})
	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}

	// Both report token_type=input as separate series (distinct "agent"), so collect by agent
	// to prove neither absorbed the other's number.
	met, ok := collectMetric(t, reader, "gen_ai.client.token.usage")
	if !ok {
		t.Fatal("gen_ai.client.token.usage never recorded")
	}
	sum, ok := met.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
	}
	byAgentInput := map[string]int64{}
	for _, dp := range sum.DataPoints {
		if attrVal(dp.Attributes, otelobs.GenAITokenType) != otelobs.GenAITokenTypeInput {
			continue
		}
		byAgentInput[attrVal(dp.Attributes, "agent")] = dp.Value
	}
	if got := byAgentInput["code-implementer"]; got != 100 {
		t.Errorf("ACP round (code-implementer) input tokens = %d, want 100 - the native call's 200 must not have blended in", got)
	}
	if got := byAgentInput["web-researcher"]; got != 200 {
		t.Errorf("native call (web-researcher) input tokens = %d, want 200 - the ACP round's 100 must not have blended in", got)
	}
}

// TestRound_EachModelCallIsAnLLMCall: every per-call usage_update becomes one llm.call ledger event with the
// round's coords, cached split out and priced, and one usage event the node sums; the answer event adds none.
func TestRound_EachModelCallIsAnLLMCall(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
	a := usageTestAgent(t, "usage-calls", &config.ModelPricing{InputPerMTok: 1, OutputPerMTok: 10})
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "c1", Node: "code-reviewer-1", Round: "worker-r0"})

	var prompt, cached, completion int32
	err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "review", artifactsrc.Artifact{}, "", "", "", "", func(s eventSpec) bool {
		if s.usage != nil {
			prompt, cached, completion = prompt+s.usage.PromptTokenCount, cached+s.usage.CachedContentTokenCount, completion+s.usage.CandidatesTokenCount
		}
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if prompt != 2500 || cached != 2000 || completion != 100 {
		t.Errorf("node usage = prompt %d cached %d completion %d, want 2500/2000/100", prompt, cached, completion)
	}

	var calls []map[string]attribute.Value
	for _, r := range capExp.records {
		attrs := map[string]attribute.Value{}
		r.WalkAttributes(func(kv attribute.KeyValue) bool { attrs[string(kv.Key)] = kv.Value; return true })
		if attrs[otelobs.GenAIOperationName].AsString() == otelobs.GenAIOperationChat {
			calls = append(calls, attrs)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("got %d llm.call events, want 2 (one per model call)", len(calls))
	}
	c := calls[1]
	for k, want := range map[string]string{otelobs.GenAIAgentName: "code-implementer", otelobs.GenAIRequestModel: "qwen3-coder",
		otelobs.GenAIConversationID: "c1", "quack.node": "code-reviewer-1", "quack.round": "worker-r0"} {
		if got := c[k].AsString(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for k, want := range map[string]int64{otelobs.GenAIUsageInputTokens: 300, otelobs.GenAIUsageCachedTokens: 1200, otelobs.GenAIUsageOutputTokens: 60} {
		if got := c[k].AsInt64(); got != want {
			t.Errorf("%s = %d, want %d", k, got, want)
		}
	}
	if got, want := c[otelobs.GenAIUsageCost].AsFloat64(), (1500*1+60*10)/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// TestRound_EachModelCallRecordsMetrics: per-call usage reaches gen_ai.client.token.usage, quack.model.requests
// and gen_ai.client.cost with native attributes, and the round's PromptResponse.Usage is not added on top.
func TestRound_EachModelCallRecordsMetrics(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage-calls", &config.ModelPricing{InputPerMTok: 1, OutputPerMTok: 10})
	a.SetLedgerCoords(ledger.Coords{Agent: "code-reviewer", User: "local", Source: "github"})
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "c1", Node: "code-reviewer-1"})

	if err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "review", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	points := tokenUsagePoints(t, reader)
	for typ, want := range map[string]int64{otelobs.GenAITokenTypeInput: 500, otelobs.GenAITokenTypeCached: 2000, otelobs.GenAITokenTypeOutput: 100} {
		dp, ok := points[typ]
		if !ok || dp.Value != want {
			t.Errorf("token_type=%q = %d (present %v), want %d summed over both calls only", typ, dp.Value, ok, want)
			continue
		}
		for k, v := range map[string]string{otelobs.GenAIRequestModel: "qwen3-coder", "agent": "code-reviewer", "user": "local", "source": "github", "role": otelobs.RoleWorker} {
			if got := attrVal(dp.Attributes, k); got != v {
				t.Errorf("token_type=%q %s = %q, want %q", typ, k, got, v)
			}
		}
	}
	if met, ok := collectMetric(t, reader, "quack.model.requests"); !ok || met.Data.(metricdata.Sum[int64]).DataPoints[0].Value != 2 {
		t.Errorf("quack.model.requests = %+v, want 2 (one per model call)", met.Data)
	}
	dp, ok := costPoint(t, reader)
	if want := (2500*1 + 100*10) / 1e6; !ok || math.Abs(dp.Value-want) > 1e-12 {
		t.Errorf("gen_ai.client.cost = %v (present %v), want %v", dp.Value, ok, want)
	}
}
