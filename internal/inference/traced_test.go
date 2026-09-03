package inference

import (
	"context"
	"errors"
	"iter"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
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

// TestTracedModel_GenerateContentRecordsGatewayFailure proves the #1105 wire:
// a generate() error still reaches the chat+node failure tracker even though
// ADK's own runner later swallows the returned error into an empty node
// completion - this is the only place the real cause survives that.
func TestTracedModel_GenerateContentRecordsGatewayFailure(t *testing.T) {
	const chatID, node = "chat-traced-1105", "write-plan"
	t.Cleanup(func() { ClearFailure(chatID, node) })

	wantErr := errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`)
	tm := &tracedModel{LLM: &stubModel{name: "m", err: wantErr}, name: "m"}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID, Node: node})

	for range tm.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	if err, streak, _, ok := LastFailure(chatID, node); !ok || streak != 1 || err.Error() != wantErr.Error() {
		t.Fatalf("LastFailure = (%v, %d, ok=%v), want the gateway error recorded once", err, streak, ok)
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

// usageEmbeddableStub additionally implements usageEmbedder.
type usageEmbeddableStub struct {
	stubModel
	vectors [][]float32
	usage   openaimodel.EmbedUsage
	err     error
}

func (e *usageEmbeddableStub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, _, err := e.EmbedWithUsage(ctx, texts)
	return vecs, err
}

func (e *usageEmbeddableStub) EmbedWithUsage(ctx context.Context, texts []string) ([][]float32, openaimodel.EmbedUsage, error) {
	if e.err != nil {
		return nil, openaimodel.EmbedUsage{}, e.err
	}
	return e.vectors, e.usage, nil
}

// TestTracedModel_Embed_RecordsTokenUsageInput pins the embeddings shape:
// only token_type=input is ever recorded (no output/reasoning/cached), and a
// call with no ctx coords falls back to defaultAgent - the same fallback rule
// GenerateContent uses.
func TestTracedModel_Embed_RecordsTokenUsageInput(t *testing.T) {
	reader := newUsageTestMeter(t)

	inner := &usageEmbeddableStub{
		stubModel: stubModel{name: "qwen3-embed"},
		vectors:   [][]float32{{1, 2, 3}},
		usage:     openaimodel.EmbedUsage{PromptTokens: 12, TotalTokens: 12},
	}
	tm := &tracedModel{LLM: inner, name: "qwen3-embed", defaultAgent: "embed"}

	if _, err := tm.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	points := tokenUsagePoints(t, reader)
	dp, ok := points[otelobs.GenAITokenTypeInput]
	if !ok {
		t.Fatalf("no data point for token_type=input (got %v)", points)
	}
	if dp.Value != 12 {
		t.Errorf("token_type=input value = %d, want 12", dp.Value)
	}
	if got := attrVal(dp.Attributes, otelobs.GenAIRequestModel); got != "qwen3-embed" {
		t.Errorf("model = %q, want qwen3-embed", got)
	}
	if got := attrVal(dp.Attributes, "agent"); got != "embed" {
		t.Errorf("agent = %q, want embed (the defaultAgent fallback)", got)
	}
	for _, typ := range []string{otelobs.GenAITokenTypeOutput, otelobs.GenAITokenTypeReasoning, otelobs.GenAITokenTypeCached} {
		if _, ok := points[typ]; ok {
			t.Errorf("token_type=%q recorded for an embed call, want only input", typ)
		}
	}
}

// TestTracedModel_Embed_RealCoordsWinOverDefault guards the fallback
// direction for Embed the same way GenerateContent's own test does: a
// per-round Coords.Agent must win over defaultAgent, never be replaced by it.
func TestTracedModel_Embed_RealCoordsWinOverDefault(t *testing.T) {
	reader := newUsageTestMeter(t)

	inner := &usageEmbeddableStub{
		stubModel: stubModel{name: "qwen3-embed"},
		vectors:   [][]float32{{1}},
		usage:     openaimodel.EmbedUsage{PromptTokens: 5, TotalTokens: 5},
	}
	tm := &tracedModel{LLM: inner, name: "qwen3-embed", defaultAgent: "embed"}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "web-researcher"})

	if _, err := tm.Embed(ctx, []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	points := tokenUsagePoints(t, reader)
	if got := attrVal(points[otelobs.GenAITokenTypeInput].Attributes, "agent"); got != "web-researcher" {
		t.Errorf("agent = %q, want web-researcher (defaultAgent must not override real Coords.Agent)", got)
	}
}

// TestTracedModel_Embed_RecordsDuration pins that an embed call lands in the
// same quack.model.call.duration histogram GenerateContent uses.
func TestTracedModel_Embed_RecordsDuration(t *testing.T) {
	reader := newUsageTestMeter(t)

	inner := &usageEmbeddableStub{stubModel: stubModel{name: "qwen3-embed"}, vectors: [][]float32{{1}}}
	tm := &tracedModel{LLM: inner, name: "qwen3-embed"}

	if _, err := tm.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	met, ok := collectMetric(t, reader, "quack.model.call.duration")
	if !ok {
		t.Fatal("quack.model.call.duration was never recorded for Embed")
	}
	hist, ok := met.Data.(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) == 0 {
		t.Fatal("quack.model.call.duration has no data points")
	}
	if got := attrVal(hist.DataPoints[0].Attributes, otelobs.GenAIRequestModel); got != "qwen3-embed" {
		t.Errorf("model = %q, want qwen3-embed", got)
	}
}

// TestTracedModel_Embed_NoUsage_NoPanic guards the defensive path: a response
// with no usage (PromptTokens==0, the EmbedUsage zero value) must record no
// token/cost metric and must not panic - the same "never fabricate a zero"
// rule recordUsageMetrics follows for a nil UsageMetadata.
func TestTracedModel_Embed_NoUsage_NoPanic(t *testing.T) {
	reader := newUsageTestMeter(t)

	inner := &usageEmbeddableStub{stubModel: stubModel{name: "qwen3-embed"}, vectors: [][]float32{{1}}}
	tm := &tracedModel{LLM: inner, name: "qwen3-embed", pricing: &config.ModelPricing{InputPerMTok: 1}}

	if _, err := tm.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if _, ok := collectMetric(t, reader, "gen_ai.client.token.usage"); ok {
		t.Error("gen_ai.client.token.usage was recorded for a usage-less embeddings response")
	}
	if _, ok := costPoint(t, reader); ok {
		t.Error("gen_ai.client.cost was recorded for a usage-less embeddings response")
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

// TestTracedModel_CachedExceedsPrompt_ClampsInputToZero guards a malformed
// provider response (cached > prompt total) from producing a negative
// token_type=input value on the hot path every model call traverses.
func TestTracedModel_CachedExceedsPrompt_ClampsInputToZero(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(10, 50, 5, 0)}}, name: "m"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	points := tokenUsagePoints(t, reader)
	if got := points[otelobs.GenAITokenTypeInput].Value; got != 0 {
		t.Errorf("token_type=input = %d, want 0 (clamped, not negative)", got)
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

// TestTracedModel_DefaultAgentFillsTokenUsageWhenCoordsCarryNone pins the
// orchestrator's own top-level turn, which never runs inside a DAG node and
// so never gets a ctx-carried Coords.Agent.
func TestTracedModel_DefaultAgentFillsTokenUsageWhenCoordsCarryNone(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(10, 0, 5, 0)}}, name: "m", defaultAgent: "orchestrator"}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{User: "local", Source: "app"})
	for range tm.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	points := tokenUsagePoints(t, reader)
	dp, ok := points[otelobs.GenAITokenTypeInput]
	if !ok {
		t.Fatal("no data point for token_type=input")
	}
	if got := attrVal(dp.Attributes, "agent"); got != "orchestrator" {
		t.Errorf("agent = %q, want orchestrator (the defaultAgent fallback)", got)
	}
}

// TestTracedModel_DefaultAgentNeverOverridesRealCoords guards the fallback
// direction: a per-round Coords.Agent (a DAG node's worker/judge call) must
// win over defaultAgent, never be replaced by it.
func TestTracedModel_DefaultAgentNeverOverridesRealCoords(t *testing.T) {
	reader := newUsageTestMeter(t)

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(10, 0, 5, 0)}}, name: "m", defaultAgent: "orchestrator"}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "code-implementer"})
	for range tm.GenerateContent(ctx, &model.LLMRequest{}, true) {
	}

	points := tokenUsagePoints(t, reader)
	if got := attrVal(points[otelobs.GenAITokenTypeInput].Attributes, "agent"); got != "code-implementer" {
		t.Errorf("agent = %q, want code-implementer (defaultAgent must not override a real Coords.Agent)", got)
	}
}

// TestTracedModel_DefaultAgentNeverLeaksIntoChatEvent guards #617: replay's
// StreamKey needs the root chat event's Coords.Agent to stay empty.
func TestTracedModel_DefaultAgentNeverLeaksIntoChatEvent(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	tm := &tracedModel{LLM: &stubModel{name: "m", resps: []*model.LLMResponse{usageResp(10, 0, 5, 0)}}, name: "m", defaultAgent: "orchestrator"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d chat log records, want 1", len(capExp.records))
	}
	attrs := attrsOf(t, capExp.records[0])
	if v, ok := attrs["gen_ai.agent.name"]; ok {
		t.Errorf("gen_ai.agent.name = %q, want absent - defaultAgent must never leak into the chat ledger event", v.AsString())
	}
}
