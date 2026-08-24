package otelobs

import "sync/atomic"

// captureContent gates span attributes carrying prompt/tool/response content
// (observability.otel.capture_content, default off) - shared by
// internal/inference and internal/acp so both span-decoration paths honor
// one flag. Set once at startup, read on every span decoration.
var captureContent atomic.Bool

// SetCaptureContent wires observability.otel.capture_content in - call once
// at startup, before serving traffic.
func SetCaptureContent(enabled bool) { captureContent.Store(enabled) }

// CaptureContentEnabled reports whether span content capture is on.
func CaptureContentEnabled() bool { return captureContent.Load() }
