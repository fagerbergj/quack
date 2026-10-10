package tools

import (
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// ProcessRequest twice leaves exactly the wrapper in the tools map, with a Declaration identical to the
// inner tool's: wrapping never changes what the model sees.
func TestEmitTool_ProcessRequestIsIdempotentAndPreservesDeclaration(t *testing.T) {
	inner := &fakeRunnable{}
	wrapped := emitWrap(inner, ledger.Coords{})
	e, ok := wrapped.(*emitTool)
	if !ok {
		t.Fatalf("emitWrap(%T) = %T, want *emitTool", inner, wrapped)
	}

	req := &model.LLMRequest{Tools: map[string]any{"risky_op": inner}}
	if err := e.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest (1st): %v", err)
	}
	if err := e.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest (2nd): %v", err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("req.Tools has %d entries, want exactly 1: %v", len(req.Tools), req.Tools)
	}
	gotTool, ok := req.Tools["risky_op"].(*emitTool)
	if !ok || gotTool != e {
		t.Errorf("req.Tools[%q] = %T, want the SAME *emitTool wrapper both times", "risky_op", req.Tools["risky_op"])
	}

	// fakeRunnable.Declaration allocates per call, so compare content, not identity.
	gotDecl := e.Declaration()
	wantDecl := inner.Declaration()
	if gotDecl.Name != wantDecl.Name || gotDecl.Description != wantDecl.Description {
		t.Errorf("Declaration() = %+v, want it to match inner.Declaration() = %+v", gotDecl, wantDecl)
	}
}
