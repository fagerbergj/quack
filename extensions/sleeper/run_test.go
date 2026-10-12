package sleeper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
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

// TestDispatchTrackedClearsRunningOnError: a synchronous Dispatch failure
// must not leave a phantom Running mark with nothing left to clear it.
func TestDispatchTrackedClearsRunningOnError(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}, dispatchErr: errors.New("boom")}
	e, _ := newTestExtension(t, host.sdkHost(), config{})
	const chatID = "ext:sleeper:test:chat"
	if err := e.dispatchTracked(context.Background(), sdk.DispatchRequest{}, chatID); err == nil {
		t.Fatal("want the Dispatch error back")
	}
	if e.isRunning(chatID) {
		t.Error("a synchronous Dispatch error must clear the running mark")
	}
}
