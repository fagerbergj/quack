package otelobs

import "sync/atomic"

// captureContent gates prompt/tool/response span attributes (observability.otel.capture_content, default off)
// for both inference and acp. Set once at startup.
var captureContent atomic.Bool

// SetCaptureContent wires observability.otel.capture_content in - call once
// at startup, before serving traffic.
func SetCaptureContent(enabled bool) { captureContent.Store(enabled) }

// CaptureContentEnabled reports whether span content capture is on.
func CaptureContentEnabled() bool { return captureContent.Load() }
