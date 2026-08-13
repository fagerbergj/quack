package serve

import (
	"context"
	"iter"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// fakeRunObserver counts RunEnded calls - enough to prove an interrupted run
// never reaches it.
type fakeRunObserver struct{ ended atomic.Int64 }

func (*fakeRunObserver) Tools() []tool.Tool                    { return nil }
func (*fakeRunObserver) RegisterRoutes(chi.Router, chi.Router) {}
func (f *fakeRunObserver) RunEnded(string, extsdk.RunOutcome)  { f.ended.Add(1) }

var _ extsdk.Extension = (*fakeRunObserver)(nil)
var _ extsdk.RunObserver = (*fakeRunObserver)(nil)

func newShutdownTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// TestDriveExtensionRunEvents_InterruptedSkipsRunEnded proves a run whose
// context dies via Hub.MarkInterrupted+CancelRun never reaches RunEnded and
// gets stamped RunStatusInterrupted instead.
func TestDriveExtensionRunEvents_InterruptedSkipsRunEnded(t *testing.T) {
	st := newShutdownTestStore(t)
	hub := stream.NewHub()
	chatID := "ext:noop:interrupt-test"
	if err := st.SetChatOrigin(context.Background(), chatID, "ext", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}

	obs := &fakeRunObserver{}
	var ext extsdk.Extension = obs
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&ext)

	entered := make(chan struct{})
	run := func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		return func(yield func(stream.SSEEvent, error) bool) {
			close(entered)
			<-runCtx.Done() // blocks until the drain's force-cancel fires
		}
	}

	done := make(chan struct{})
	go func() {
		driveExtensionRunEvents(context.Background(), "noop", nil, st, hub, &extHolder, "ext", chatID, "turn-1", 0, run)
		close(done)
	}()

	<-entered
	if !hub.HasRegisteredRun(chatID) {
		t.Fatal("run not registered with the hub while in flight")
	}
	hub.MarkInterrupted(chatID)
	hub.CancelRun(chatID)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("driveExtensionRunEvents did not return after cancel")
	}

	if n := obs.ended.Load(); n != 0 {
		t.Errorf("RunEnded called %d times, want 0 (shutdown must not notify the extension)", n)
	}
	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus != store.RunStatusInterrupted {
		t.Errorf("RunStatus = %q, want %q", c.RunStatus, store.RunStatusInterrupted)
	}
	if c.ActiveTurnID != "" {
		t.Errorf("ActiveTurnID = %q, want cleared", c.ActiveTurnID)
	}
}

// TestDrainActiveRuns_FinishesWithinGrace proves the common case: a run that
// completes on its own before the grace window elapses is left alone -
// never marked interrupted, never force-cancelled.
func TestDrainActiveRuns_FinishesWithinGrace(t *testing.T) {
	hub := stream.NewHub()
	chatID := "chat-finishes"
	cancelled := false
	_, cancel := context.WithCancel(context.Background())
	hub.RegisterRun(chatID, "turn-1", func() { cancelled = true; cancel() })

	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.UnregisterRun(chatID) // simulates the run's own goroutine finishing normally
	}()

	DrainActiveRuns(hub, 2*time.Second)

	if cancelled {
		t.Error("run was force-cancelled despite finishing within the grace window")
	}
	if hub.WasInterrupted(chatID) {
		t.Error("run marked interrupted despite finishing on its own")
	}
	if hub.Draining() != true {
		t.Error("Draining() should be true once DrainActiveRuns has run")
	}
}

// TestDrainActiveRuns_ForceCancelsPastGrace proves the escalation path: a
// run still registered once grace elapses is marked interrupted and its
// cancel func is invoked.
func TestDrainActiveRuns_ForceCancelsPastGrace(t *testing.T) {
	hub := stream.NewHub()
	chatID := "chat-stuck"
	cancelCh := make(chan struct{})
	unregistered := make(chan struct{})
	hub.RegisterRun(chatID, "turn-1", func() { close(cancelCh) })
	go func() {
		<-cancelCh
		hub.UnregisterRun(chatID)
		close(unregistered)
	}()

	DrainActiveRuns(hub, 100*time.Millisecond)

	select {
	case <-unregistered:
	default:
		t.Fatal("run was not force-cancelled and cleaned up within the settle window")
	}
	if !hub.WasInterrupted(chatID) {
		t.Error("run past grace should have been marked interrupted before the force-cancel")
	}
}

// TestHubDrainingRejectsDispatch is a focused unit check on the flag itself -
// BeginDraining/Draining - independent of any HTTP wiring.
func TestHubDrainingRejectsDispatch(t *testing.T) {
	hub := stream.NewHub()
	if hub.Draining() {
		t.Fatal("hub reports draining before BeginDraining")
	}
	hub.BeginDraining()
	if !hub.Draining() {
		t.Error("hub does not report draining after BeginDraining")
	}
}
