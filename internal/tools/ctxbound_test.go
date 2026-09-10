package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// blockingRunnable never returns on its own and never looks at ctx - the
// case ctxBoundTool exists to bound regardless (a raw blocking call, or a
// third-party client that doesn't thread context through).
type blockingRunnable struct {
	mu      sync.Mutex
	runs    int
	started chan struct{}
	release chan struct{}
}

func newBlockingRunnable() *blockingRunnable {
	return &blockingRunnable{started: make(chan struct{}), release: make(chan struct{})}
}

func (*blockingRunnable) Name() string        { return "blocking_op" }
func (*blockingRunnable) Description() string { return "blocks until released" }
func (*blockingRunnable) IsLongRunning() bool { return false }
func (*blockingRunnable) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "blocking_op"}
}
func (*blockingRunnable) ProcessRequest(adkagent.Context, *model.LLMRequest) error { return nil }
func (b *blockingRunnable) Run(adkagent.Context, any) (map[string]any, error) {
	b.mu.Lock()
	b.runs++
	b.mu.Unlock()
	close(b.started)
	<-b.release // never selects on ctx.Done()
	return map[string]any{"ok": true}, nil
}

// newCancellableFakeCtx is fakeCtx with a caller-controlled Ctx, since
// newFakeCtx hardcodes context.Background() (never cancellable).
func newCancellableFakeCtx(ctx context.Context) *fakeCtx {
	return &fakeCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: ctx}, state: &fakeState{m: map[string]any{}}}
}

// TestCtxBoundToolReturnsOnCancelEvenWhenInnerIgnoresContext: `chat stop`
// cancels the run's context, but a tool that never checks ctx would keep the
// round hung regardless - ctxBoundTool must return on cancel either way.
func TestCtxBoundToolReturnsOnCancelEvenWhenInnerIgnoresContext(t *testing.T) {
	inner := newBlockingRunnable()
	bt, ok := newCtxBoundTool(inner).(*ctxBoundTool)
	if !ok {
		t.Fatalf("newCtxBoundTool returned %T, want *ctxBoundTool", newCtxBoundTool(inner))
	}

	cctx, cancel := context.WithCancel(context.Background())
	ctx := newCancellableFakeCtx(cctx)

	errc := make(chan error, 1)
	go func() {
		_, err := bt.Run(ctx, map[string]any{})
		errc <- err
	}()

	<-inner.started // the inner call is genuinely in flight, not refused/short-circuited
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel - a stop would hang on this tool call")
	}
	close(inner.release) // unblock the leaked goroutine so it doesn't outlive the test
}

// TestCtxBoundToolPassesThroughOnSuccess: the common, uncancelled path is unaffected.
func TestCtxBoundToolPassesThroughOnSuccess(t *testing.T) {
	inner := &fakeRunnable{}
	bt := newCtxBoundTool(inner)
	ctx := newFakeCtx()

	out, err := bt.(*ctxBoundTool).Run(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("Run result = %v, want ok:true", out)
	}
	if inner.runCount() != 1 {
		t.Fatalf("inner ran %d times, want 1", inner.runCount())
	}
}
