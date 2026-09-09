package openaimodel

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// recordingHandler captures the last record handled, for asserting on log
// level without a logging test-helper dependency.
type recordingHandler struct{ rec *slog.Record }

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.rec = r
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestApiErrLogsAtErrorByDefault covers onboarding audit finding 14's other
// half indirectly: apiErr's ERROR log is the load-bearing failure signal for
// a normal (non-best-effort) caller and must stay at Error.
func TestApiErrLogsAtErrorByDefault(t *testing.T) {
	var got slog.Record
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(recordingHandler{rec: &got}))

	o := &OpenAIModel{ModelName: "m"}
	_ = o.apiErr(context.Background(), "generate", errors.New("dial tcp: connection refused"))

	if got.Level != slog.LevelError {
		t.Errorf("level = %v, want Error for a normal caller", got.Level)
	}
}

// TestApiErrLogsAtDebugForBestEffort covers onboarding audit finding 14: a
// best-effort caller (chat title generation) already logs its own degraded
// outcome, so apiErr's own boundary log must not also fire at Error - that
// was the second (unlabelled, no trace_id) ERROR line the audit found.
func TestApiErrLogsAtDebugForBestEffort(t *testing.T) {
	var got slog.Record
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(recordingHandler{rec: &got}))

	o := &OpenAIModel{ModelName: "m"}
	ctx := WithBestEffort(context.Background())
	_ = o.apiErr(ctx, "generate", errors.New("dial tcp: connection refused"))

	if got.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug for a WithBestEffort caller", got.Level)
	}
}
