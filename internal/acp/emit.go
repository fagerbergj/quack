package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// acpScope names the logger every invoke_agent ledger event is emitted
// through.
const acpScope = "quack.acp"

// maxTeeBytes bounds each direction's captured conversation - a full
// protocol transcript, not a tail (see tailBuffer's stderr use, which wants
// the opposite): a long-running round's early handshake matters as much as
// its last message, so capture stops rather than slides once the cap is hit.
const maxTeeBytes = 4 << 20 // 4 MiB per direction

// teeBuffer is an io.Writer that accumulates up to maxTeeBytes bytes,
// dropping (not truncating from the front) whatever comes after the cap -
// the whole point is "record everything up to a bound", not "record the
// tail" (contrast tailBuffer in proc.go).
type teeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	n := len(p) // reported below regardless of how much we actually keep
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := maxTeeBytes - t.buf.Len(); room > 0 {
		if room < len(p) {
			p = p[:room]
		}
		t.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never errors
	}
	// ALWAYS report the full original write consumed: this Writer is paired
	// with the real transport via io.MultiWriter (stdin), which treats any
	// short count as io.ErrShortWrite and fails the whole write - a capture
	// buffer must never be able to break the connection it's tapping.
	return n, nil
}

// lines splits the captured ndjson (one JSON-RPC message per line, ACP's
// wire framing) into a []json.RawMessage - the shape both a JSON array
// attribute and a lenient reader want. A trailing partial line (buffer cap
// hit mid-message) is dropped, not emitted malformed.
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

// emitInvokeAgent records one gen_ai "invoke_agent" ledger event per ACP
// subprocess round (internal/acp's own per-round process lifecycle - see
// the package doc), carrying the full teed protocol conversation: sent is
// what quack wrote to the subprocess's stdin, received is what it read back
// from stdout. Coordinates (conversation/node/round) come off ctx - the
// SAME ledger.Coords the vetting gate stamped before invoking this agent's
// RunNode, since an ACP round runs inside that same call tree.
func emitInvokeAgent(ctx context.Context, agentName string, sent, received *teeBuffer, roundErr error) {
	if !otelobs.LoggingEnabled(acpScope) {
		return // nothing listening - skip parsing/marshaling the teed conversation
	}
	attrs := []otellog.KeyValue{
		otellog.String(otelobs.GenAIOperationName, otelobs.GenAIOperationInvokeAgent),
		otellog.String(otelobs.GenAIAgentName, agentName),
	}
	if b, err := json.Marshal(sent.lines()); err == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIInputMessages, string(b)))
	}
	if b, err := json.Marshal(received.lines()); err == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIOutputMessages, string(b)))
	}
	if roundErr != nil {
		attrs = append(attrs, otellog.String(otelobs.ErrorType, roundErr.Error()))
	}
	otelobs.EmitLog(ctx, acpScope, "", attrs...)
}
