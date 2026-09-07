package agent

import (
	"context"
	"iter"
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

// probeTraceTool records the trace id its invocation ctx carries - simulating
// a worker-side otelobs span (e.g. a model call) that should be a child of
// whatever span the caller had open when it dispatched over A2A.
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

// probeModel: turn 1 calls the probe tool; turn 2 (after the tool result) answers.
type probeModel struct{}

func (probeModel) Name() string { return "probe-model" }

func (probeModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	sawToolResult := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawToolResult = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if sawToolResult {
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "done"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "probe", Args: map[string]any{}}},
			}},
			TurnComplete: true,
		}, nil)
	}
}

// newProbeWorker builds an llmagent whose only tool is the trace probe.
func newProbeWorker(t *testing.T, got *string) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "probe-worker",
		Description: "A worker that probes its trace context.",
		Model:       probeModel{},
		Instruction: "Call the probe tool then answer.",
		Tools:       []tool.Tool{probeTraceTool(t, got)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// TestA2APropagatesTraceContext is the reproduction for #1046: a "run" span
// opened before dispatching to a worker over the loopback A2A boundary must
// still be the ancestor of spans the worker opens while handling the request.
// Today it is not - each per-node A2A server starts handling the HTTP request
// with a bare context, so the worker's spans root a brand new trace.
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

	srv, err := Serve(ag, session.InMemoryService(), nil, Compaction{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node")
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
