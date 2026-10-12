package sleeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
)

func TestExtensionSatisfiesRunObserver(t *testing.T) {
	var _ sdk.RunObserver = (*extension)(nil)
}

func TestOriginLabel(t *testing.T) {
	cases := []struct{ stop, job, partner, want string }{
		{"3", "lineup", "", "Lineup · week 3"},
		{"3", "waivers", "", "Waivers · week 3"},
		{"3", "digest", "", "Digest · week 3"},
		{"draft", "draft", "", "Draft"},
		{"review", "history", "", "Season review"},
		{"3", "trade", "Rice Cooker", "Trade · Rice Cooker"},
		{"3", "trade-finder", "", "Trade finder · week 3"},
	}
	for _, c := range cases {
		if got := originLabel(c.stop, c.job, c.partner); got != c.want {
			t.Errorf("originLabel(%q,%q,%q) = %q, want %q", c.stop, c.job, c.partner, got, c.want)
		}
	}
}

// TestHandleArtifactsReportsRunning: Dispatch marks a chat running, RunEnded clears it.
func TestHandleArtifactsReportsRunning(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	e, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"lineup"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, body = %s", rec.Code, rec.Body)
	}

	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Jobs["lineup"].Running {
		t.Fatal("jobs[lineup].Running = false right after dispatch, want true")
	}

	e.RunEnded(wantChatID, sdk.RunOutcome{Status: sdk.RunDone})

	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp3 artifactsResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp3.Jobs["lineup"].Running {
		t.Error("jobs[lineup].Running = true after RunEnded, want false")
	}
}

func TestRunEndedUnknownChatIsNoop(t *testing.T) {
	e := &extension{}
	e.RunEnded("ext:sleeper:nope", sdk.RunOutcome{Status: sdk.RunFailed})
	if e.isRunning("ext:sleeper:nope") {
		t.Error("an unknown chat should never end up marked running")
	}
}

// TestDispatchTrackedMarksRunningBeforeDispatch: a Dispatch that fires RunEnded before returning must
// not leave the chat marked running.
func TestDispatchTrackedMarksRunningBeforeDispatch(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	e, _ := newTestExtension(t, host.sdkHost(), config{})
	const chatID = "ext:sleeper:test:chat"
	var runningDuringDispatch bool
	host.onDispatch = func() {
		runningDuringDispatch = e.isRunning(chatID)
		e.RunEnded(chatID, sdk.RunOutcome{}) // fast run ends inside Dispatch
	}
	if err := e.dispatchTracked(context.Background(), sdk.DispatchRequest{}, chatID); err != nil {
		t.Fatalf("dispatchTracked: %v", err)
	}
	if !runningDuringDispatch {
		t.Error("chatID must be marked running before Dispatch is called, not after it returns")
	}
	if e.isRunning(chatID) {
		t.Error("a RunEnded fired inside Dispatch must clear the mark, not resurrect it")
	}
}

// TestDispatchTrackedRunningOnError: a failed Dispatch clears the mark, or nothing would; a busy
// refusal keeps it, since the chat's live run is still going and its RunEnded clears it.
func TestDispatchTrackedRunningOnError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantRunning bool
	}{
		{"failed", errors.New("boom"), false},
		{"busy", fmt.Errorf("chat x %w", sdk.ErrRunInFlight), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeHost{artifacts: map[string]map[string][]byte{}, dispatchErr: tc.err}
			e, _ := newTestExtension(t, host.sdkHost(), config{})
			const chatID = "ext:sleeper:test:chat"
			if err := e.dispatchTracked(context.Background(), sdk.DispatchRequest{}, chatID); !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the Dispatch error back", err)
			}
			if got := e.isRunning(chatID); got != tc.wantRunning {
				t.Errorf("isRunning = %v, want %v", got, tc.wantRunning)
			}
		})
	}
}

// notesHost refuses the first season-notes dispatch as busy and records every dispatch; the retry
// arrives on a goroutine, hence the lock and channel.
type notesHost struct {
	mu       sync.Mutex
	firstErr error
	calls    []string
	called   chan struct{}
}

func (h *notesHost) dispatch(_ context.Context, req sdk.DispatchRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, req.Chat.LocalID)
	h.called <- struct{}{}
	err := h.firstErr
	h.firstErr = nil
	return err
}

func TestSeasonNotesBusyRedispatchesAfterItsRunEnded(t *testing.T) {
	host := &notesHost{firstErr: fmt.Errorf("chat x %w", sdk.ErrRunInFlight), called: make(chan struct{}, 4)}
	e, _ := newTestExtension(t, sdk.Host{Dispatch: host.dispatch}, config{})
	notesLocal := testLeague + ":season-notes"
	notesChat := globalChatID(notesLocal)

	e.dispatchSeasonNotes(context.Background(), testLeague)
	<-host.called
	if !e.isRunning(notesChat) {
		t.Error("a busy season-notes chat should stay marked running")
	}
	e.RunEnded(globalChatID(testLeague+":2:trends"), sdk.RunOutcome{Status: sdk.RunDone})
	e.runningMu.Lock()
	armed := e.notesRetry[notesChat]
	e.runningMu.Unlock()
	if armed != testLeague {
		t.Fatalf("notesRetry[%s] = %q after another chat's RunEnded, want %q still armed", notesChat, armed, testLeague)
	}

	e.RunEnded(notesChat, sdk.RunOutcome{Status: sdk.RunDone})
	select {
	case <-host.called:
	case <-time.After(5 * time.Second):
		t.Fatal("season notes were not redispatched after their chat's run ended")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.calls) != 2 || host.calls[1] != notesLocal {
		t.Errorf("dispatches = %v, want the refused one then its retry on %s", host.calls, notesLocal)
	}
}

func TestSeasonNotesFailureArmsNoRetry(t *testing.T) {
	host := &notesHost{firstErr: errors.New("boom"), called: make(chan struct{}, 4)}
	e, _ := newTestExtension(t, sdk.Host{Dispatch: host.dispatch}, config{})
	notesChat := globalChatID(testLeague + ":season-notes")
	e.dispatchSeasonNotes(context.Background(), testLeague)
	if _, armed := e.takeNotesRetry(notesChat); armed {
		t.Error("only a busy refusal should arm a season-notes retry")
	}
}
