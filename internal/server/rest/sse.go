package rest

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/fagerbergj/quack/internal/stream"
)

// sseWriter writes Quack's event vocabulary as Server-Sent Events, flushing
// after each so the client receives them incrementally. The framing
// (`event: <name>\ndata: <json>\n\n`) is what the frontend's readAgentStream parses.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter sets SSE headers and returns a writer, or ok=false if the
// ResponseWriter cannot flush.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// send writes one event and flushes.
func (s *sseWriter) send(ev stream.SSEEvent) error {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Name, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
