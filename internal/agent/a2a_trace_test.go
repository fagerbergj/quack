package agent

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// probeArgs is the (empty) input for the trace-probe tool.
type probeArgs struct{}

// probeTraceTool records its ctx's trace id, standing in for a worker-side span that should descend from the
// caller's span open at A2A dispatch.
func probeTraceTool(t *testing.T, got *string) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[probeArgs, string](
		functiontool.Config{Name: "probe", Description: "Records the current trace id."},
		func(ac adkagent.Context, _ probeArgs) (string, error) {
			_, span := otelobs.Start(ac, "probe")
			defer span.End()
			*got = span.SpanContext().TraceID().String()
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// probeModel calls the probe tool, then answers once it sees the result.
var probeModel = &fakeLLM{func(req *model.LLMRequest) *model.LLMResponse {
	if len(funcResponses(req)) > 0 {
		return turn(&genai.Part{Text: "done"})
	}
	return turn(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "probe", Args: map[string]any{}}})
}}

// newProbeWorker builds an llmagent whose only tool is the trace probe.
func newProbeWorker(t *testing.T, got *string) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "probe-worker",
		Description: "A worker that probes its trace context.",
		Model:       probeModel,
		Instruction: "Call the probe tool then answer.",
		Tools:       []tool.Tool{probeTraceTool(t, got)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// A "run" span opened before an A2A dispatch must remain the ancestor of the worker's handling spans.
func TestA2APropagatesTraceContext(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotTraceID string
	ag := newProbeWorker(t, &gotTraceID)

	srv, err := Serve(ag, session.InMemoryService(), nil, nil, Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "spike",
		Agent:             client,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the orchestrator's "run" root span, open BEFORE the A2A dispatch.
	ctx, runSpan := otelobs.Start(context.Background(), "run")
	wantTraceID := runSpan.SpanContext().TraceID().String()

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	for _, err := range r.Run(ctx, "local", "s1", content, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	runSpan.End()

	if gotTraceID == "" {
		t.Fatal("probe tool never ran; test setup is broken")
	}
	if gotTraceID != wantTraceID {
		t.Errorf("worker-side span trace id = %s, want the run span's trace id %s (context did not propagate across the A2A boundary)", gotTraceID, wantTraceID)
	}
}
