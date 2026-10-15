package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/genai"

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

// emitLLMCall records one ACP model call as an llm.call ledger event, the shape inference.emitChatEvent gives a
// native call: the agent calls its endpoint directly, so quack's inference layer never sees it.
func (a *Agent) emitLLMCall(ctx context.Context, u *genai.GenerateContentResponseUsageMetadata) {
	if !otelobs.LoggingEnabled(acpScope) {
		return
	}
	cached, prompt, output := int64(u.CachedContentTokenCount), int64(u.PromptTokenCount), int64(u.CandidatesTokenCount)
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationChat),
		attribute.String(otelobs.GenAIProviderName, otelobs.GenAIProviderOpenAI),
		attribute.String(otelobs.GenAIRequestModel, a.opts.ModelName),
		attribute.String(otelobs.GenAIAgentName, a.name),
		attribute.Int64(otelobs.GenAIUsageInputTokens, max(prompt-cached, 0)), // excludes cached, as natively
		attribute.Int64(otelobs.GenAIUsageCachedTokens, cached),
		attribute.Int64(otelobs.GenAIUsageOutputTokens, output),
	}
	if a.opts.Pricing != nil {
		attrs = append(attrs, attribute.Float64(otelobs.GenAIUsageCost, a.opts.Pricing.Cost(prompt, cached, output)))
	}
	otelobs.EmitLog(ctx, acpScope, "", attrs...)
}

// recordTool records one finished ACP tool call (a pairSpec) as tools.Hooks.after records a native one: a
// tool.call ledger row on the round's stream plus the tool-call counter. Other specs are a no-op.
func (a *Agent) recordTool(ctx context.Context, coords ledger.Coords, spec eventSpec) {
	if len(spec.parts) != 2 || spec.parts[0].FunctionCall == nil || spec.parts[1].FunctionResponse == nil {
		return
	}
	call, resp := spec.parts[0].FunctionCall, spec.parts[1].FunctionResponse.Response
	var result any = resp
	var err error
	if msg, failed := resp["error"].(string); failed {
		result, err = nil, errors.New(msg)
	}
	otelobs.RecordToolCall(coords, call.Name, err != nil)
	c := ledger.CoordsFromContext(ctx)
	c.Agent = a.name // the stream emitInvokeAgent and emitLLMCall write to
	otelobs.EmitToolCall(ledger.WithCoords(ctx, c), acpScope, call.Name, call.Args, result, err)
}
