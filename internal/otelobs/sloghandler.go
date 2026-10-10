package otelobs

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// WrapHandler appends trace_id/span_id to records whose ctx carries a valid span; otherwise it is a pure
// passthrough, so output without a spanned ctx is byte-for-byte unchanged.
func WrapHandler(next slog.Handler) slog.Handler { return &spanHandler{next: next} }

type spanHandler struct{ next slog.Handler }

func (h *spanHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *spanHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h *spanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &spanHandler{next: h.next.WithAttrs(attrs)}
}

func (h *spanHandler) WithGroup(name string) slog.Handler {
	return &spanHandler{next: h.next.WithGroup(name)}
}
