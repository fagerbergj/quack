package openaimodel

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// recordingHandler captures the last record handled.
type recordingHandler struct{ rec *slog.Record }

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.rec = r
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

func withRecordingLogger(t *testing.T) *slog.Record {
	t.Helper()
	var got slog.Record
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(recordingHandler{rec: &got}))
	return &got
}

func TestApiErrLogsAtErrorByDefault(t *testing.T) {
	got := withRecordingLogger(t)
	o := &OpenAIModel{ModelName: "m"}
	_ = o.apiErr(context.Background(), "generate", errors.New("dial tcp: connection refused"))
	if got.Level != slog.LevelError {
		t.Errorf("level = %v, want Error for a normal caller", got.Level)
	}
}

func TestApiErrLogsAtDebugForBestEffort(t *testing.T) {
	got := withRecordingLogger(t)
	o := &OpenAIModel{ModelName: "m"}
	ctx := WithBestEffort(context.Background())
	_ = o.apiErr(ctx, "generate", errors.New("dial tcp: connection refused"))
	if got.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug for a WithBestEffort caller", got.Level)
	}
}

// TestApiErrLogsAtDebugWhenInProcess pins the CLI-duck case: the caller
// already prints the failure, so the boundary log would just repeat it.
func TestApiErrLogsAtDebugWhenInProcess(t *testing.T) {
	got := withRecordingLogger(t)
	prevInProcess := inProcess.Load()
	t.Cleanup(func() { inProcess.Store(prevInProcess) })
	SetInProcess()

	o := &OpenAIModel{ModelName: "m"}
	_ = o.apiErr(context.Background(), "generate", errors.New("dial tcp: connection refused"))
	if got.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug when SetInProcess was called", got.Level)
	}
}
