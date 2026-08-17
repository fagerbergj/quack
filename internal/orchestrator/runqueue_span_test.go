package orchestrator

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func withQueueTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

func queueSpans(exp *tracetest.InMemoryExporter) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		if s.Name() == "quack.run.queue" {
			out = append(out, s)
		}
	}
	return out
}

// A run that waits on a full semaphore and is cancelled must leave a
// quack.run.queue span - otherwise its quack.run root is childless and its
// whole latency reads as work.
func TestAcquireRunSpansTheWait(t *testing.T) {
	exp := withQueueTracer(t)
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)

	release, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("first acquire should take the only slot")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, got := o.acquireRun(ctx); got {
		t.Fatal("second acquire should not have acquired on a cancelled ctx")
	}

	spans := queueSpans(exp)
	if len(spans) != 1 {
		t.Fatalf("quack.run.queue spans = %d, want 1 (only the blocked wait)", len(spans))
	}
	for _, a := range spans[0].Attributes() {
		if a.Key == "acquired" {
			if a.Value.AsBool() {
				t.Error("acquired = true, want false for a cancelled wait")
			}
			return
		}
	}
	t.Error("quack.run.queue is missing the acquired attribute")
}

// The waited-then-acquired shape. Without this, `acquired` could be hardcoded
// false and the cancel test above would still pass - inverting the
// "execution = quack.run minus quack.run.queue" arithmetic for every real wait.
func TestAcquireRunSpansTheWaitThenAcquires(t *testing.T) {
	exp := withQueueTracer(t)
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)

	hold, ok := o.acquireRun(context.Background())
	if !ok {
		t.Fatal("first acquire should take the only slot")
	}
	got := make(chan bool, 1)
	go func() {
		rel, acq := o.acquireRun(context.Background())
		rel()
		got <- acq
	}()
	time.Sleep(50 * time.Millisecond)
	hold()
	if acq := <-got; !acq {
		t.Fatal("waiter did not acquire after the slot freed")
	}

	spans := queueSpans(exp)
	if len(spans) != 1 {
		t.Fatalf("quack.run.queue spans = %d, want 1", len(spans))
	}
	acquired, found := false, false
	for _, a := range spans[0].Attributes() {
		if a.Key == "acquired" {
			found, acquired = true, a.Value.AsBool()
		}
	}
	if !found || !acquired {
		t.Errorf("acquired = %v (found=%v), want true for a wait that acquired", acquired, found)
	}
}

// The uncontended path stays unspanned: a slot that is free costs nothing.
func TestAcquireRunDoesNotSpanTheFastPath(t *testing.T) {
	exp := withQueueTracer(t)
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)

	release, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("uncontended acquire should succeed")
	}
	release()

	if n := len(queueSpans(exp)); n != 0 {
		t.Fatalf("quack.run.queue spans = %d, want 0 when the slot is free", n)
	}
}
