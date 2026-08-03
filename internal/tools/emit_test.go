package tools

import (
	"testing"

	"google.golang.org/adk/v2/model"
)

// TestEmitTool_ProcessRequestIsIdempotentAndPreservesDeclaration answers the
// review question on emitTool.ProcessRequest's req.Tools[e.Name()] = e
// re-pointing (mirrors guard.go/cancelguard.go's established convention):
// calling it twice on the same request must leave exactly the WRAPPER in the
// map (never duplicate entries, never revert to the inner tool), and the
// LLM-visible Declaration must be byte-identical to the inner tool's -
// wrapping must never change what the model sees.
func TestEmitTool_ProcessRequestIsIdempotentAndPreservesDeclaration(t *testing.T) {
	inner := &fakeRunnable{}
	wrapped, err := emitWrap(inner)
	if err != nil {
		t.Fatalf("emitWrap: %v", err)
	}
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

	// fakeRunnable.Declaration allocates a fresh struct per call (not a cached
	// pointer), so identity isn't the right check - content is: the wrapper
	// must return exactly what the inner tool declares, untouched.
	gotDecl := e.Declaration()
	wantDecl := inner.Declaration()
	if gotDecl.Name != wantDecl.Name || gotDecl.Description != wantDecl.Description {
		t.Errorf("Declaration() = %+v, want it to match inner.Declaration() = %+v", gotDecl, wantDecl)
	}
}
