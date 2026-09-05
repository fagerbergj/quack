package serve

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestMapExtRunOutcome pins the extsdk.RunStatus mapping (#879's quack
// half): cancelled must win regardless of what the DB-derived status says,
// and the pre-existing failed/needs_input/done rules stay put.
func TestMapExtRunOutcome(t *testing.T) {
	needsInput := stream.NodeNeedsInputData{NodeID: "node-7"}
	tests := []struct {
		name       string
		status     string
		cancelled  bool
		timedOut   bool
		wantStatus extsdk.RunStatus
	}{
		{"cancelled beats failed", store.RunStatusFailed, true, false, extsdk.RunCancelled},
		{"cancelled beats needs_input", store.RunStatusNeedsInput, true, false, extsdk.RunCancelled},
		{"cancelled beats idle/done", store.RunStatusIdle, true, false, extsdk.RunCancelled},
		{"failed maps through", store.RunStatusFailed, false, false, extsdk.RunFailed},
		{"needs_input maps through", store.RunStatusNeedsInput, false, false, extsdk.RunNeedsInput},
		{"idle maps to done", store.RunStatusIdle, false, false, extsdk.RunDone},
		{"timed out, not cancelled, still done", store.RunStatusIdle, false, true, extsdk.RunDone},
		{"cancelled beats timed out too", store.RunStatusIdle, true, true, extsdk.RunCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := mapExtRunOutcome(tt.status, "what next?", "", "partial answer", true, needsInput, tt.timedOut, tt.cancelled)
			if out.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", out.Status, tt.wantStatus)
			}
			if out.Answer != "partial answer" {
				t.Errorf("Answer = %q, want the answer preserved regardless of status", out.Answer)
			}
			if tt.wantStatus == extsdk.RunNeedsInput {
				if out.Question != "what next?" || out.NodeID != "node-7" {
					t.Errorf("Question/NodeID = %q/%q, want populated for RunNeedsInput", out.Question, out.NodeID)
				}
			}
		})
	}
}

// TestMapExtRunOutcome_FailedWithNodeErrorFillsError is sdk v0.10.0's fix:
// RunOutcome.Error carries a failed run's real cause; Answer stays empty
// rather than folding the cause into it (formerly #1105's stopgap).
func TestMapExtRunOutcome_FailedWithNodeErrorFillsError(t *testing.T) {
	needsInput := stream.NodeNeedsInputData{}
	out := mapExtRunOutcome(store.RunStatusFailed, "", "model gateway returned 502 Bad Gateway on 5 consecutive attempts over 48m0s",
		"", true, needsInput, false, false)
	if out.Status != extsdk.RunFailed {
		t.Fatalf("Status = %q, want RunFailed", out.Status)
	}
	if out.Answer != "" {
		t.Errorf("Answer = %q, want empty - the cause belongs in Error", out.Answer)
	}
	if !strings.Contains(out.Error, "502 Bad Gateway") || !strings.Contains(out.Error, "5 consecutive attempts") {
		t.Errorf("Error = %q, want it to name the error class and attempt count", out.Error)
	}
}

// TestMapExtRunOutcome_TrueSilentGapErrorStaysEmpty proves the #568 path is
// untouched: no failed-node error means Error stays empty for the
// extension's own silent-gap text.
func TestMapExtRunOutcome_TrueSilentGapErrorStaysEmpty(t *testing.T) {
	needsInput := stream.NodeNeedsInputData{}
	out := mapExtRunOutcome(store.RunStatusIdle, "", "", "", true, needsInput, false, false)
	if out.Answer != "" {
		t.Fatalf("Answer = %q, want empty for a genuine silent gap", out.Answer)
	}
	if out.Error != "" {
		t.Fatalf("Error = %q, want empty for a genuine silent gap", out.Error)
	}
}

// cancelOutcomeObserver captures the single RunOutcome a test run ends with.
type cancelOutcomeObserver struct {
	outcome atomic.Pointer[extsdk.RunOutcome]
}

func (*cancelOutcomeObserver) Tools() []tool.Tool                    { return nil }
func (*cancelOutcomeObserver) RegisterRoutes(chi.Router, chi.Router) {}
func (o *cancelOutcomeObserver) RunEnded(_ string, outcome extsdk.RunOutcome) {
	o.outcome.Store(&outcome)
}

var (
	_ extsdk.Extension   = (*cancelOutcomeObserver)(nil)
	_ extsdk.RunObserver = (*cancelOutcomeObserver)(nil)
)

// TestDriveExtensionRunEvents_UserCancelReportsRunCancelled proves the
// seam a user Stop actually goes through: hub.CancelRun (DELETE /chats/{id},
// and the same runHandle a Stop-button PATCH cancels) reaches the in-flight
// run as ctx.Canceled, and RunEnded must receive extsdk.RunCancelled - not
// RunDone with whatever partial answer the run left behind (#879).
func TestDriveExtensionRunEvents_UserCancelReportsRunCancelled(t *testing.T) {
	st, orch, hub, _, _ := newExtTestStack(t)
	chatID := "ext:cancel-test:user-stop"
	if err := st.SetChatOrigin(context.Background(), chatID, "ext", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}

	obs := &cancelOutcomeObserver{}
	var ext extsdk.Extension = obs
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&ext)

	entered := make(chan struct{})
	run := func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		return func(yield func(stream.SSEEvent, error) bool) {
			close(entered)
			<-runCtx.Done() // held open until the test's hub.CancelRun fires
		}
	}

	done := make(chan struct{})
	go func() {
		driveExtensionRunEvents(context.Background(), "cancel-test", orch, st, hub, &extHolder, "ext", chatID, "turn-1", 0, run)
		close(done)
	}()

	<-entered
	if !hub.HasRegisteredRun(chatID) {
		t.Fatal("run not registered with the hub while in flight")
	}
	if !hub.CancelRun(chatID) {
		t.Fatal("CancelRun found no registered run to cancel")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("driveExtensionRunEvents did not return after cancel")
	}

	got := obs.outcome.Load()
	if got == nil {
		t.Fatal("RunEnded was never called")
	}
	if got.Status != extsdk.RunCancelled {
		t.Errorf("Status = %q, want %q", got.Status, extsdk.RunCancelled)
	}
}
