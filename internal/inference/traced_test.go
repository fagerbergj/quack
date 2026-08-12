package inference

import (
	"context"
	"errors"
	"iter"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// stubModel is a minimal model.LLM for testing tracedModel's passthrough.
type stubModel struct {
	name  string
	resps []*model.LLMResponse
	err   error
}

func (s *stubModel) Name() string { return s.name }

func (s *stubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, r := range s.resps {
			if !yield(r, nil) {
				return
			}
		}
		if s.err != nil {
			yield(nil, s.err)
		}
	}
}

// embeddableStub additionally implements Embedder.
type embeddableStub struct {
	stubModel
	vectors [][]float32
	err     error
}

func (e *embeddableStub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.vectors, nil
}

func TestTracedModel_NamePassthrough(t *testing.T) {
	tm := &tracedModel{LLM: &stubModel{name: "qwen3-coder"}, name: "qwen3-coder"}
	if got := tm.Name(); got != "qwen3-coder" {
		t.Errorf("Name() = %q, want qwen3-coder", got)
	}
}

func TestTracedModel_GenerateContentPassesThroughAllResponses(t *testing.T) {
	want := []*model.LLMResponse{{}, {}, {}}
	tm := &tracedModel{LLM: &stubModel{name: "m", resps: want}, name: "m"}

	var got []*model.LLMResponse
	for r, err := range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d responses, want %d", len(got), len(want))
	}
}

func TestTracedModel_GenerateContentPassesThroughError(t *testing.T) {
	wantErr := errors.New("boom")
	tm := &tracedModel{LLM: &stubModel{name: "m", err: wantErr}, name: "m"}

	var gotErr error
	for _, err := range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			gotErr = err
		}
	}
	if !errors.Is(gotErr, wantErr) {
		t.Errorf("error = %v, want %v", gotErr, wantErr)
	}
}

func TestTracedModel_GenerateContentStopsEarlyOnConsumerBreak(t *testing.T) {
	resps := []*model.LLMResponse{{}, {}, {}}
	tm := &tracedModel{LLM: &stubModel{name: "m", resps: resps}, name: "m"}

	n := 0
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		n++
		if n == 1 {
			break
		}
	}
	if n != 1 {
		t.Errorf("consumed %d responses, want the loop to stop after 1", n)
	}
}

func TestTracedModel_EmbedDelegatesWhenSupported(t *testing.T) {
	want := [][]float32{{1, 2, 3}}
	inner := &embeddableStub{stubModel: stubModel{name: "embed-model"}, vectors: want}
	tm := &tracedModel{LLM: inner, name: "embed-model"}

	got, err := tm.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 3 {
		t.Errorf("Embed() = %v, want %v", got, want)
	}
}

func TestTracedModel_EmbedErrorsWhenUnsupported(t *testing.T) {
	tm := &tracedModel{LLM: &stubModel{name: "no-embed"}, name: "no-embed"}
	if _, err := tm.Embed(context.Background(), []string{"hello"}); err == nil {
		t.Error("expected an error for a model that doesn't implement Embedder")
	}
}

// Ensure tracedModel satisfies Embedder (NewEmbedder's type assertion relies on this).
var _ Embedder = (*tracedModel)(nil)

// newUsageTestMeter installs a fresh manual-reader-backed meter as the
// otelobs package singleton, so tracedModel's token/cost Record calls land
// somewhere this test can read back.
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

// tokenUsagePoints indexes gen_ai.client.token.usage's current data points
// by their gen_ai.token.type attribute.
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

func usageResp(prompt, cached, candidates, thoughts int32) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Parts: []*genai.Part{{Text: "answer"}}},
		TurnComplete: true,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        prompt,
			CachedContentTokenCount: cached,
			CandidatesTokenCount:    candidates,
			ThoughtsTokenCount:      thoughts,
		},
	}
}

// TestTracedModel_TokenUsage_SplitsCachedFromInput pins the token_type
// fan-out: genai's PromptTokenCount already includes cached tokens, so
// token_type=input must report the NON-cached remainder or input+cached
// would double-count a cache hit. Attribution (agent/user/source) comes
// through ctx, exactly like emitChatEvent's coords.
func TestTracedModel_TokenUsage_SplitsCachedFromInput(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(100, 30, 40, 5)}}, name: "m"}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "code-implementer", User: "local", Source: "github"})
	for range tm.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	points := tokenUsagePoints(t, reader)
	want := map[string]int64{
		otelobs.GenAITokenTypeInput:     70, // 100 prompt total - 30 cached
		otelobs.GenAITokenTypeOutput:    40,
		otelobs.GenAITokenTypeReasoning: 5,
		otelobs.GenAITokenTypeCached:    30,
	}
	for typ, wantN := range want {
		dp, ok := points[typ]
		if !ok {
			t.Fatalf("no data point for token_type=%q (got %v)", typ, points)
		}
		if dp.Value != wantN {
			t.Errorf("token_type=%q value = %d, want %d", typ, dp.Value, wantN)
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

// TestTracedModel_Cost_ComputedFromConfiguredPricing pins the price math:
// cost = raw prompt total (cached billed at the input rate, no separate
// cached tier configured) * input price + (output+reasoning) * output price.
func TestTracedModel_Cost_ComputedFromConfiguredPricing(t *testing.T) {
	reader := newUsageTestMeter(t)

	pricing := &config.ModelPricing{InputPerMTok: 2, OutputPerMTok: 4}
	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(2_000_000, 500_000, 1_000_000, 0)}}, name: "m", pricing: pricing}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "code-implementer"})
	for range tm.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	dp, ok := costPoint(t, reader)
	if !ok {
		t.Fatal("gen_ai.client.cost was never recorded")
	}
	// 2M prompt tokens * $2/Mtok + 1M output tokens * $4/Mtok = $8.
	const want = 8.0
	if dp.Value != want {
		t.Errorf("gen_ai.client.cost = %v, want %v", dp.Value, want)
	}
	if got := attrVal(dp.Attributes, otelobs.GenAIRequestModel); got != "m" {
		t.Errorf("gen_ai.client.cost gen_ai.request.model = %q, want m", got)
	}
}

// TestTracedModel_NoPricing_NoCostMetric guards "never guess a price": a
// model absent from config's price table gets token usage but no cost.
func TestTracedModel_NoPricing_NoCostMetric(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(100, 0, 40, 0)}}, name: "m"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	if _, ok := costPoint(t, reader); ok {
		t.Error("gen_ai.client.cost was recorded for a model with no configured pricing, want none")
	}
	if points := tokenUsagePoints(t, reader); points[otelobs.GenAITokenTypeInput].Value != 100 {
		t.Errorf("token usage still expected without pricing, got %v", points)
	}
}

// TestTracedModel_NoUsageMetadata_NoMetrics guards a response that never
// carried usage (a provider outage, a malformed reply) - no metric at all,
// never a fabricated zero.
func TestTracedModel_NoUsageMetadata_NoMetrics(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{{Content: &genai.Content{Parts: []*genai.Part{{Text: "answer"}}}, TurnComplete: true}}}, name: "m"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	if _, ok := collectMetric(t, reader, "gen_ai.client.token.usage"); ok {
		t.Error("gen_ai.client.token.usage was recorded for a response with no UsageMetadata")
	}
	if _, ok := costPoint(t, reader); ok {
		t.Error("gen_ai.client.cost was recorded for a response with no UsageMetadata")
	}
}

// TestTracedModel_AttributionAbsentWhenCoordsUnset guards "omit, don't
// guess": a call with no ledger coords on ctx must not stamp empty-string
// agent/user/source attributes.
func TestTracedModel_AttributionAbsentWhenCoordsUnset(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(10, 0, 0, 0)}}, name: "m"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	points := tokenUsagePoints(t, reader)
	dp, ok := points[otelobs.GenAITokenTypeInput]
	if !ok {
		t.Fatal("no data point for token_type=input")
	}
	for _, key := range []string{"agent", "user", "source"} {
		if _, ok := dp.Attributes.Value(attribute.Key(key)); ok {
			t.Errorf("attribute %q is set with no coords on ctx, want it omitted", key)
		}
	}
}
