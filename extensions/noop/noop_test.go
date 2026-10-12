package noop

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

// fakeHost captures dispatched requests in memory; this module can't import quack.
type fakeHost struct {
	mu       sync.Mutex
	captured []sdk.DispatchRequest
	err      error
}

func (f *fakeHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.captured = append(f.captured, req)
	return nil
}

func newFakeHost() (*fakeHost, sdk.Host) {
	fh := &fakeHost{}
	host := sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(bytesDiscard{}, nil)),
		DataDir:  "",
	}
	return fh, host
}

type bytesDiscard struct{}

func (bytesDiscard) Write(p []byte) (int, error) { return len(p), nil }

func newRouter(ext sdk.Extension) chi.Router {
	r := chi.NewRouter()
	ext.RegisterRoutes(r, chi.NewRouter())
	return r
}

func TestFactoryDefaultGreeting(t *testing.T) {
	_, host := newFakeHost()
	ext, err := factory(host, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	r := newRouter(ext)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Greeting != "noop" {
		t.Fatalf("greeting = %q, want %q", got.Greeting, "noop")
	}
	if got.Extension != "noop" {
		t.Fatalf("extension = %q, want %q", got.Extension, "noop")
	}
}

func TestFactoryCustomGreeting(t *testing.T) {
	_, host := newFakeHost()
	ext, err := factory(host, []byte("greeting: hi there\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	r := newRouter(ext)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Greeting != "hi there" {
		t.Fatalf("greeting = %q, want %q", got.Greeting, "hi there")
	}
}

func TestFactoryRejectsBadConfig(t *testing.T) {
	_, host := newFakeHost()
	if _, err := factory(host, []byte("greeting: [not, a, string\n")); err == nil {
		t.Fatal("expected an error for malformed config, got nil")
	}
}

func TestDispatchCallsHostWithFixedShape(t *testing.T) {
	fh, host := newFakeHost()
	ext, err := factory(host, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	r := newRouter(ext)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dispatch", strings.NewReader("hello world")))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.captured) != 1 {
		t.Fatalf("captured %d requests, want 1", len(fh.captured))
	}
	got := fh.captured[0]

	if !strings.HasPrefix(got.Chat.LocalID, "noop-") {
		t.Errorf("Chat.LocalID = %q, want noop-<counter> prefix", got.Chat.LocalID)
	}
	if got.Ask.Message != "hello world" {
		t.Errorf("Ask.Message = %q, want %q", got.Ask.Message, "hello world")
	}
	if got.Run.Workflow != "" {
		t.Errorf("Run.Workflow = %q, want empty", got.Run.Workflow)
	}
	if got.Chat.Origin == nil {
		t.Fatal("Chat.Origin is nil")
	}
	if got.Chat.Origin.Extension != "noop" || got.Chat.Origin.Label != "noop test" || got.Chat.Origin.Kind != "test" {
		t.Errorf("Chat.Origin = %+v, want {noop, noop test, test}", got.Chat.Origin)
	}
}

func TestDispatchAssignsDistinctChatIDs(t *testing.T) {
	fh, host := newFakeHost()
	ext, err := factory(host, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	r := newRouter(ext)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dispatch", bytes.NewReader(nil)))
	}

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.captured) != 3 {
		t.Fatalf("captured %d requests, want 3", len(fh.captured))
	}
	seen := map[string]bool{}
	for _, req := range fh.captured {
		if seen[req.Chat.LocalID] {
			t.Fatalf("duplicate Chat.LocalID %q", req.Chat.LocalID)
		}
		seen[req.Chat.LocalID] = true
	}
}

func TestStatusCountsRunEndedNotDispatchCalls(t *testing.T) {
	_, host := newFakeHost()
	extVal, err := factory(host, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	r := newRouter(ext)

	// two dispatches, but status tracks completed runs (RunEnded), not calls made
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dispatch", bytes.NewReader(nil)))
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Dispatches != 0 {
		t.Fatalf("dispatches = %d before any RunEnded, want 0", got.Dispatches)
	}

	ext.RunEnded("noop-1", sdk.RunOutcome{Status: sdk.RunDone})
	ext.RunEnded("noop-2", sdk.RunOutcome{Status: sdk.RunFailed})

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Dispatches != 2 {
		t.Fatalf("dispatches = %d after 2 RunEnded calls, want 2", got.Dispatches)
	}
}

func TestRegisteredInInit(t *testing.T) {
	factories := sdk.Registered()
	if _, ok := factories["noop"]; !ok {
		t.Fatal(`sdk.Registered() missing "noop" - init() should have registered it`)
	}
}
