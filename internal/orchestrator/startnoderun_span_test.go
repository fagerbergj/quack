package orchestrator

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
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
	o.startNodeRun(context.Background(), "u", "c", "", "", nil, "n1", func(stream.SSEEvent, error) bool { return true })

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
	o.startNodeRun(ctx, "u", "c", "", "", nil, "n1", func(stream.SSEEvent, error) bool { return true })
	span.End()

	if n := len(runSpans(exp)); n != 1 {
		t.Fatalf("quack.run spans = %d, want 1 (only the outer one)", n)
	}
}

// TestRetryAndStartNodeStampRunCoords: retry and node-start runs file their root span (and
// every record) under the chat and user, as Run does, though they enter without Run's stamp.
func TestRetryAndStartNodeStampRunCoords(t *testing.T) {
	for name, run := range map[string]func(o *Orchestrator){
		"retry": func(o *Orchestrator) {
			for range o.RetryNode(context.Background(), "u-7", "c", "", nil, "n1", "") {
			}
		},
		"start": func(o *Orchestrator) {
			o.StartNode(context.Background(), "u-7", "c", "", "n1", "", func(stream.SSEEvent, error) bool { return true })
		},
	} {
		t.Run(name, func(t *testing.T) {
			exp := withRunTracer(t)
			sessions := session.InMemoryService()
			run(&Orchestrator{sessions: sessions, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)})
			spans := runSpans(exp)
			if len(spans) != 1 {
				t.Fatalf("quack.run spans = %d, want 1", len(spans))
			}
			var user string
			for _, kv := range spans[0].Attributes() {
				if string(kv.Key) == otelobs.UserID {
					user = kv.Value.AsString()
				}
			}
			if user != "u-7" {
				t.Errorf("run span user = %q, want u-7", user)
			}
		})
	}
	c := ledger.CoordsFromContext(runCoords(ledger.WithCoords(context.Background(), ledger.Coords{Source: "github"}), "c", "u"))
	if c.ChatID != "c" || c.User != "u" || c.Source != "github" {
		t.Errorf("runCoords = %+v, want chat/user filled and the caller's source kept", c)
	}
	if c := ledger.CoordsFromContext(runCoords(context.Background(), "c", "u")); c.Source != SourceApp {
		t.Errorf("runCoords source = %q, want %q", c.Source, SourceApp)
	}
}
