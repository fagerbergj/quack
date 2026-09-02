package orchestrator

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fagerbergj/quack/internal/stream"
	"google.golang.org/adk/v2/session"
)

func withRunTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

func runSpans(exp *tracetest.InMemoryExporter) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		if s.Name() == "quack.run" {
			out = append(out, s)
		}
	}
	return out
}

// startNodeRun errors out fast (no stashed plan in a fresh session), which
// happens after span setup - enough to prove the span decision itself.

// A bare-ctx call (StartNode's path) must still get a real "quack.run" span,
// or its trace shows no root at all.
func TestStartNodeRunSpansOnBareCtx(t *testing.T) {
	exp := withRunTracer(t)
	o := &Orchestrator{sessions: session.InMemoryService()}
	o.startNodeRun(context.Background(), "u", "c", "", nil, "n1", func(stream.SSEEvent, error) bool { return true })

	if n := len(runSpans(exp)); n != 1 {
		t.Fatalf("quack.run spans = %d, want 1 for a bare-ctx call", n)
	}
}

// A call already inside a "run" span (Run's resumeNodeRun path) must not open
// a second identically-named child - that's the run-under-run trace noise.
func TestStartNodeRunDoesNotDoubleSpanInsideRun(t *testing.T) {
	exp := withRunTracer(t)
	o := &Orchestrator{sessions: session.InMemoryService()}

	ctx, span := otel.Tracer("test").Start(context.Background(), "quack.run")
	o.startNodeRun(ctx, "u", "c", "", nil, "n1", func(stream.SSEEvent, error) bool { return true })
	span.End()

	if n := len(runSpans(exp)); n != 1 {
		t.Fatalf("quack.run spans = %d, want 1 (only the outer one)", n)
	}
}
