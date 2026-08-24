package stream

import "context"

type yieldCtxKey struct{}

// WithYield stores fn in ctx so tools called from the orchestrator can forward
// SSE events up through the outer SSE stream without going through the ADK
// session event pipeline. fn MUST already serialize its callers: concurrent DAG
// nodes call it from their own goroutines (dag/graph.go's onQueued, vetting's
// stage spans), and an unsynchronized fn panics the process (#1016, #1032).
func WithYield(ctx context.Context, fn func(SSEEvent)) context.Context {
	return context.WithValue(ctx, yieldCtxKey{}, fn)
}

// YieldFromContext retrieves the yield function stored by WithYield, or returns
// false if none was stored.
func YieldFromContext(ctx context.Context) (func(SSEEvent), bool) {
	fn, ok := ctx.Value(yieldCtxKey{}).(func(SSEEvent))
	return fn, ok
}
