package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// fakeExtensionCaller is an in-process extensionCaller: no subprocess, no
// round() goroutine, so steerForward's own logic is deterministic to test.
type fakeExtensionCaller struct {
	err           error
	gotMethod     string
	gotParamsText string
	calls         int
}

func (f *fakeExtensionCaller) CallExtension(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.calls++
	f.gotMethod = method
	if p, ok := params.(steerParams); ok {
		f.gotParamsText = p.Text
	}
	return nil, f.err
}

// TestSteerForward_DeliversOverAckedExtensionCall (#998, replaces the flaky
// #1202 round()-based e2e version: that test could not be made to fail
// deterministically after 2000+ -race runs, so per the no-flaky-gates rule
// it was deleted and replaced with this direct unit test of the same
// production closure - no subprocess, no goroutine handoff, nothing to race).
func TestSteerForward_DeliversOverAckedExtensionCall(t *testing.T) {
	fake := &fakeExtensionCaller{}
	fwd := steerForward(fake)

	if !fwd("focus on cost") {
		t.Fatal("forward reported failure for a call the fake acked")
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
	if fake.gotMethod != steerExtMethod {
		t.Fatalf("method = %q, want %q", fake.gotMethod, steerExtMethod)
	}
	if fake.gotParamsText != "focus on cost" {
		t.Fatalf("params text = %q, want %q", fake.gotParamsText, "focus on cost")
	}
}

// TestSteerForward_ReportsFailureOnRejectedCall (#998 review): the forward
// func must use an ACKED call (CallExtension), not a fire-and-forget
// notification - otherwise a steer landing after the shim has settled the
// round would report delivered while silently dropped. An error from the
// extension call must surface as false so enqueue's caller parks the
// message instead of marking it delivered.
func TestSteerForward_ReportsFailureOnRejectedCall(t *testing.T) {
	fake := &fakeExtensionCaller{err: errors.New("no live round to steer")}
	fwd := steerForward(fake)

	if fwd("too late") {
		t.Fatal("forward reported success for a steer the extension call rejected")
	}
}

// TestRound_SteerRejectedByShimReportsFailure (#998 review): the forward
// func must use an ACKED call (CallExtension), not a fire-and-forget
// notification - otherwise a steer landing in the window between the shim
// settling the round and quack's deferred Unregister would report delivered
// while silently dropped. The "steer-reject" fake agent errors every
// _quack/steer call (as the shim does once promptReq is already nil) - the
// forward func must surface that as false so enqueue's caller parks instead
// of marking the message delivered.
func TestRound_SteerRejectedByShimReportsFailure(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var forward func(string) bool
	registered := make(chan struct{})

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=steer-reject"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterLiveSteer: func(chatID, nodeID string, f func(string) bool) {
			forward = f
			close(registered)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", "chat1", "n1", "", "", func(eventSpec) bool { return true })
	}()

	<-registered
	if forward("too late") {
		t.Fatal("forward reported success for a steer the shim rejected - a caller would wrongly mark it delivered")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("round: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round never finished")
	}
}
