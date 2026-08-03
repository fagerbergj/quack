package inference

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

var errBoom = errors.New("boom")

// captureExporter records every emitted record for direct inspection - the
// simplest possible sdklog.Exporter for a unit test.
type captureExporter struct{ records []sdklog.Record }

func (c *captureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *captureExporter) Shutdown(context.Context) error   { return nil }
func (c *captureExporter) ForceFlush(context.Context) error { return nil }

func attrsOf(t *testing.T, r sdklog.Record) map[string]otellog.Value {
	t.Helper()
	out := map[string]otellog.Value{}
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

// TestTracedModel_EmitsWellFormedChatEvent is the emission-wrapper test the
// issue asks for: a fake model.LLM run through tracedModel must produce one
// gen_ai "chat" log record carrying the full request/response content, the
// coordinates from ctx, and no per-chunk noise (only the final response).
func TestTracedModel_EmitsWellFormedChatEvent(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	temp := float32(0.4)
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "you are a test agent"}}},
			Temperature:       &temp,
			MaxOutputTokens:   256,
		},
	}
	resps := []*model.LLMResponse{
		{Content: &genai.Content{Parts: []*genai.Part{{Text: "partial"}}}, Partial: true},
		{
			Content:      &genai.Content{Parts: []*genai.Part{{Text: "final answer"}}},
			TurnComplete: true,
			ModelVersion: "test-model-v1",
			FinishReason: genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     12,
				CandidatesTokenCount: 34,
			},
		},
	}
	stub := &stubModel{name: "test-model", resps: resps}
	tm := &tracedModel{LLM: stub, name: "test-model"}

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-1", Node: "n1", Agent: "worker", Round: "worker-r0"})
	for range tm.GenerateContent(ctx, req, true) {
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d emitted records, want exactly 1", len(capExp.records))
	}
	attrs := attrsOf(t, capExp.records[0])

	if got := attrs["gen_ai.operation.name"].AsString(); got != "chat" {
		t.Errorf("gen_ai.operation.name = %q, want chat", got)
	}
	if got := attrs["gen_ai.request.model"].AsString(); got != "test-model" {
		t.Errorf("gen_ai.request.model = %q, want test-model", got)
	}
	if got := attrs["gen_ai.conversation.id"].AsString(); got != "chat-1" {
		t.Errorf("gen_ai.conversation.id = %q, want chat-1", got)
	}
	if got := attrs["quack.node"].AsString(); got != "n1" {
		t.Errorf("quack.node = %q, want n1", got)
	}
	if got := attrs["quack.round"].AsString(); got != "worker-r0" {
		t.Errorf("quack.round = %q, want worker-r0", got)
	}
	if got := attrs["gen_ai.agent.name"].AsString(); got != "worker" {
		t.Errorf("gen_ai.agent.name = %q, want worker", got)
	}

	var input []*genai.Content
	if err := json.Unmarshal([]byte(attrs["gen_ai.input.messages"].AsString()), &input); err != nil {
		t.Fatalf("gen_ai.input.messages not valid JSON: %v", err)
	}
	if len(input) != 1 || input[0].Parts[0].Text != "hello" {
		t.Errorf("gen_ai.input.messages = %+v, want the request contents", input)
	}

	var output genai.Content
	if err := json.Unmarshal([]byte(attrs["gen_ai.output.messages"].AsString()), &output); err != nil {
		t.Fatalf("gen_ai.output.messages not valid JSON: %v", err)
	}
	if output.Parts[0].Text != "final answer" {
		t.Errorf("gen_ai.output.messages = %q, want the FINAL response only (not the partial chunk)", output.Parts[0].Text)
	}
	if got := attrs["gen_ai.response.model"].AsString(); got != "test-model-v1" {
		t.Errorf("gen_ai.response.model = %q, want test-model-v1", got)
	}
	if v := attrs["gen_ai.usage.input_tokens"].AsInt64(); v != 12 {
		t.Errorf("gen_ai.usage.input_tokens = %v, want 12", v)
	}
	if v := attrs["gen_ai.usage.output_tokens"].AsInt64(); v != 34 {
		t.Errorf("gen_ai.usage.output_tokens = %v, want 34", v)
	}
	finishReasons := attrs["gen_ai.response.finish_reasons"].AsSlice()
	if len(finishReasons) != 1 || finishReasons[0].AsString() != string(genai.FinishReasonStop) {
		t.Errorf("gen_ai.response.finish_reasons = %v, want [%q]", finishReasons, genai.FinishReasonStop)
	}

	if attrs["gen_ai.system_instructions"].AsString() == "" {
		t.Error("gen_ai.system_instructions missing")
	}
	if attrs["gen_ai.prompt.version"].AsString() == "" {
		t.Error("gen_ai.prompt.version (content hash) missing")
	}
	if v := float32(attrs["gen_ai.request.temperature"].AsFloat64()); v != 0.4 {
		t.Errorf("gen_ai.request.temperature = %v, want 0.4", v)
	}
	if v := attrs["gen_ai.request.max_tokens"].AsInt64(); v != 256 {
		t.Errorf("gen_ai.request.max_tokens = %v, want 256", v)
	}
	if _, ok := attrs["error.type"]; ok {
		t.Error("error.type present on a successful call")
	}
}

func TestTracedModel_EmitsErrorType(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	stub := &stubModel{name: "m", err: errBoom}
	tm := &tracedModel{LLM: stub, name: "m"}
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := attrsOf(t, capExp.records[0])
	if attrs["error.type"].AsString() != errBoom.Error() {
		t.Errorf("error.type = %q, want %q", attrs["error.type"].AsString(), errBoom.Error())
	}
}
