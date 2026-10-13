package serve

import (
	"context"
	"iter"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/runlog"
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

// TestDriveExtensionRunEvents_InterruptedSkipsRunEnded: a run killed via Hub.MarkInterrupted+CancelRun never reaches
// RunEnded or sends done. Cut before it planned, it keeps its in-flight marker for boot to settle.
func TestDriveExtensionRunEvents_InterruptedSkipsRunEnded(t *testing.T) {
	st := newShutdownTestStore(t)
	hub := stream.NewHub()
	chatID := "ext:noop:interrupt-test"
	if err := st.SetChatOrigin(context.Background(), chatID, "ext", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	_ = st.MarkRunActive(context.Background(), chatID, "turn-1") // the dispatch's own mark

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
		driveExtensionRunEvents(context.Background(), "noop", nil, st, hub, runlog.NewEventLog(st), &extHolder, "ext", chatID, "turn-1", 0, run)
		close(done)
	}()

	<-entered
	if !hub.HasRegisteredRun(chatID) {
		t.Fatal("run not registered with the hub while in flight")
	}
	_, live, unsubscribe, _ := hub.Subscribe(chatID)
	defer unsubscribe()
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
	for ev := range live {
		if ev.SSE.Name == stream.EventDone {
			t.Error("a subscriber got done; a watching client would stop following the resumed run")
		}
	}
	evs, err := st.LoadChatEvents(context.Background(), chatID, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("LoadChatEvents: %d events, err %v", len(evs), err)
	}
	for _, e := range evs {
		if strings.Contains(e.Event, `"name":"done"`) {
			t.Errorf("persisted done at seq %d; a replaying client would stop following the resumed run", e.Seq)
		}
	}
	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus == store.RunStatusPaused || c.ActiveTurnID != "turn-1" {
		t.Errorf("RunStatus, ActiveTurnID = %q, %q, want not paused and turn-1 kept for boot", c.RunStatus, c.ActiveTurnID)
	}
	if _, settled, _, err := st.ScanOrphanedRuns(context.Background()); err != nil || len(settled) != 1 || settled[0] != chatID {
		t.Errorf("boot scan settled %v (%v), want [%s]", settled, err, chatID)
	}
}

// TestDrainActiveRuns_FinishesWithinGrace: a run completing before the grace window ends is never
// marked interrupted or force-cancelled.
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

	DrainActiveRuns(hub, nil, 2*time.Second)

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

// TestDrainActiveRuns_ForceCancelsPastGrace: a run still registered once grace elapses is marked
// interrupted and its cancel func invoked.
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

	DrainActiveRuns(hub, nil, 100*time.Millisecond)

	// unregistered closes on its own goroutine, one statement after the
	// hub state DrainActiveRuns polled on; wait for it instead of racing it.
	select {
	case <-unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("run was not force-cancelled and cleaned up within the settle window")
	}
	// A force-cancel sets the per-chat cut marker so the run's tail skips RunEnded and stamps the chat
	// paused; runs finishing on their own never get it.
	if !hub.WasInterrupted(chatID) {
		t.Error("force-cancel did not set the per-chat cut marker")
	}
}

// TestDrainActiveRuns_CatchesRunRegisteredAfterSnapshot: a run registered after the drain's one-time
// ActiveChatIDs snapshot is still waited for and force-cancelled, and chat-known ending early can't end the loop.
func TestDrainActiveRuns_CatchesRunRegisteredAfterSnapshot(t *testing.T) {
	hub := stream.NewHub()
	hub.RegisterRun("chat-known", "turn-known", func() {})

	lateCancelled := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		hub.RegisterRun("chat-late", "turn-late", func() { close(lateCancelled) })
	}()
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.UnregisterRun("chat-known")
	}()
	unregistered := make(chan struct{})
	go func() {
		<-lateCancelled
		hub.UnregisterRun("chat-late") // the run's own cleanup, like FinishRun's defer, once cancel lands
		close(unregistered)
	}()

	DrainActiveRuns(hub, nil, 300*time.Millisecond)

	select {
	case <-unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("a run registered after the drain snapshot was never force-cancelled")
	}
	if hub.HasRegisteredRun("chat-late") {
		t.Error("late run still registered after drain returned")
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

// fixedPauser reports one live node per chat and pauses it when pausable says so.
type fixedPauser struct{ pausable map[string]bool }

func (p fixedPauser) ActiveNodes(string) []string { return []string{"n1"} }
func (fixedPauser) MarkShutdown(string)           {}
func (p fixedPauser) PauseNodeForShutdown(chatID, _ string, mark func(string)) bool {
	if p.pausable[chatID] {
		mark(chatID)
	}
	return p.pausable[chatID]
}

// finishingPauser models a node that ends between ActiveNodes and its pause: the run's tail reads the cut
// mark (as stampRunOutcome does) and unregisters, then the pause finds no control.
type finishingPauser struct {
	hub        *stream.Hub
	tailSawCut bool
}

func (*finishingPauser) ActiveNodes(string) []string { return []string{"n1"} }
func (*finishingPauser) MarkShutdown(string)         {}
func (p *finishingPauser) PauseNodeForShutdown(chatID, _ string, _ func(string)) bool {
	p.tailSawCut = p.hub.WasInterrupted(chatID)
	p.hub.UnregisterRun(chatID)
	return false
}

// A run whose last node finishes as the drain reaches it ended on its own: its tail must not read it as cut.
func TestDrainActiveRuns_NodeFinishingBeforePauseIsNotCut(t *testing.T) {
	hub := stream.NewHub()
	hub.RegisterRun("chat", "turn-1", func() {})
	p := &finishingPauser{hub: hub}

	DrainActiveRuns(hub, p, 2*time.Second)

	if p.tailSawCut {
		t.Error("a run that finished on its own was read as cut by shutdown; its RunEnded would be skipped")
	}
}

// A shutdown pause ends a run inside the grace, so a chat whose node it paused is marked cut and its tail
// skips RunEnded; a chat with nothing paused keeps a normal ending.
func TestDrainActiveRuns_MarksAChatWhoseNodeItPausedAsCut(t *testing.T) {
	hub := stream.NewHub()
	for _, id := range []string{"paused", "finishing"} {
		hub.RegisterRun(id, "turn-1", func() {})
		go func(id string) {
			time.Sleep(50 * time.Millisecond)
			hub.UnregisterRun(id) // each run ends inside the grace
		}(id)
	}

	DrainActiveRuns(hub, fixedPauser{pausable: map[string]bool{"paused": true}}, 2*time.Second)

	if !hub.WasInterrupted("paused") {
		t.Error("a chat whose node the drain paused was not marked cut; its tail would deliver RunEnded")
	}
	if hub.WasInterrupted("finishing") {
		t.Error("a chat with nothing paused was marked cut; its normal RunEnded would be skipped")
	}
}
