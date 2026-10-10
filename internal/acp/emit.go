package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// acpScope names the logger every invoke_agent ledger event is emitted
// through.
const acpScope = "quack.acp"

// maxTeeBytes bounds each direction's captured transcript. Capture stops rather than slides at the cap:
// a round's early handshake matters as much as its last message (unlike tailBuffer's stderr).
const maxTeeBytes = 4 << 20 // 4 MiB per direction

// teeBuffer accumulates up to maxTeeBytes and drops whatever comes after (contrast tailBuffer in proc.go).
type teeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// reset clears the buffer between a pinned process's rounds; each round owns its own slice of the wire.
func (t *teeBuffer) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Reset()
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	n := len(p) // reported below regardless of how much we actually keep
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := maxTeeBytes - t.buf.Len(); room > 0 {
		if room < len(p) {
			p = p[:room]
		}
		t.buf.Write(p)
	}
	// Always report the full write consumed: io.MultiWriter treats a short count as ErrShortWrite,
	// and a capture buffer must never break the connection it taps.
	return n, nil
}

// lines splits the captured ndjson (one JSON-RPC message per line) into []json.RawMessage.
// A trailing partial line (cap hit mid-message) is dropped, not emitted malformed.
func (t *teeBuffer) lines() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []json.RawMessage
	for _, line := range bytes.Split(t.buf.Bytes(), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	return out
}

// emitInvokeAgent records one invoke_agent ledger event per ACP round with the teed conversation (sent = stdin,
// received = stdout). Coords come off ctx: the gate stamped them before calling RunNode, in this call tree.
func emitInvokeAgent(ctx context.Context, agentName string, sent, received *teeBuffer, roundErr error, plugins []ledger.PluginRef, artifacts []ledger.ArtifactRef) {
	if !otelobs.LoggingEnabled(acpScope) {
		return // nothing listening - skip parsing/marshaling the teed conversation
	}
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationInvokeAgent),
		attribute.String(otelobs.GenAIAgentName, agentName),
	}
	if b, err := json.Marshal(sent.lines()); err == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIInputMessages, string(b)))
	}
	if b, err := json.Marshal(received.lines()); err == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIOutputMessages, string(b)))
	}
	if len(plugins) > 0 {
		if b, err := json.Marshal(plugins); err == nil {
			attrs = append(attrs, attribute.String(otelobs.QuackPlugins, string(b)))
		}
	}
	if len(artifacts) > 0 {
		if b, err := json.Marshal(artifacts); err == nil {
			attrs = append(attrs, attribute.String(otelobs.QuackArtifacts, string(b)))
		}
	}
	if roundErr != nil {
		attrs = append(attrs, attribute.String(otelobs.ErrorType, roundErr.Error()))
	}
	otelobs.EmitLog(ctx, acpScope, "", attrs...)
}
