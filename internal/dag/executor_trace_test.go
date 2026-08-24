package dag

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestDagStream_TraceIDFromRealSpan: node_start and worker agent_start must
// carry the RUN's real trace id (not "", not a stale/unrelated one). One trace
// covers the whole plan run - every node's span is a child of it - so two
// nodes in the same run correctly share the SAME trace id; that is not the
// old bug (the old bug was a stale captured ctx, fixed by resolving the id
// once at construction instead of storing a context.Context - Finding 4).
func TestDagStream_TraceIDFromRealSpan(t *testing.T) {
	tp := sdktrace.NewTracerProvider() // no exporter needed; span still records real ids
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "run")
	defer span.End()

	traceID := otelobs.TraceIDOf(ctx)
	if traceID == "" {
		t.Fatal("recording span produced an empty trace id - test setup is broken")
	}

	agentByID := map[string]string{"n1": "web-researcher", "n2": "code-explorer"}
	var got []stream.SSEEvent
	ds := newDagStream(traceID, agentByID,
		func(e stream.SSEEvent, _ error) bool { got = append(got, e); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) bool { return false },
		func(string, int) string { return "" },
	)

	evs := []*session.Event{
		ev("quack-dag-p@1/n1@rr/web-researcher@worker-r0", &genai.Part{Text: "n1 draft"}),
		ev("quack-dag-p@1/n2@rr/code-explorer@worker-r0", &genai.Part{Text: "n2 draft"}),
	}
	for _, e := range evs {
		if !ds.handle(e) {
			t.Fatal("handle returned false")
		}
	}

	var nodeStarts []stream.NodeStartData
	var agentStarts []stream.AgentStartData
	for _, e := range got {
		switch d := e.Data.(type) {
		case stream.NodeStartData:
			nodeStarts = append(nodeStarts, d)
		case stream.AgentStartData:
			agentStarts = append(agentStarts, d)
		}
	}

	if len(nodeStarts) != 2 || len(agentStarts) != 2 {
		t.Fatalf("want 2 node_start + 2 agent_start, got %d node_start, %d agent_start", len(nodeStarts), len(agentStarts))
	}
	for _, d := range nodeStarts {
		if d.TraceID != traceID {
			t.Errorf("node_start(%s).TraceID = %q, want the run's real trace id %q - never empty, never a stale one", d.NodeID, d.TraceID, traceID)
		}
	}
	for _, d := range agentStarts {
		if d.TraceID != traceID {
			t.Errorf("agent_start(%s).TraceID = %q, want the run's real trace id %q", d.RunID, d.TraceID, traceID)
		}
	}
}
