package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortSSEReconnect(t *testing.T) {
	t.Helper()
	orig := sseReconnectDelay
	sseReconnectDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { sseReconnectDelay = orig })
}

func writeSSE(w io.Writer, id int32, name, data string) {
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, name, data)
}

// cutRun streams a run's first two events and ends without `done`, as a shutdown drain does.
func cutRun(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeSSE(w, 1, "node_start", `{"node_id":"n1"}`)
	writeSSE(w, 2, "node_paused", `{"node_id":"n1"}`)
}

// restartedStream is GET /stream on a restarting server: unreachable, then 503 while booting, then the resumed run.
func restartedStream(t *testing.T) http.HandlerFunc {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case 2:
			http.Error(w, "booting", http.StatusServiceUnavailable)
		default:
			if got := r.Header.Get("Last-Event-ID"); got != "2" {
				t.Errorf("resume Last-Event-ID = %q, want 2", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, 3, "node_start", `{"node_id":"n1"}`)
			writeSSE(w, 4, "node_done", `{"node_id":"n1","output":"final answer"}`)
			writeSSE(w, 5, "done", `{}`)
		}
	}
}

func assertFollowedThrough(t *testing.T, code int, out, errOut string) {
	t.Helper()
	if code != 0 || !strings.Contains(out, "final answer") {
		t.Fatalf("exit %d, stdout %q; want 0 with the resumed run's answer (stderr %q)", code, out, errOut)
	}
	if got := strings.Count(errOut, "connection lost; reconnecting…"); got != 1 {
		t.Errorf("stderr = %q; want one reconnect notice for the outage", errOut)
	}
}

// A deploy cuts the followed run without `done`; -f rides out the restart and follows the resumed run to its answer.
func TestRunChatShowFollowsThroughRestart(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	shortSSEReconnect(t)
	resume := restartedStream(t)
	var attached atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, r *http.Request) {
		if attached.CompareAndSwap(false, true) {
			cutRun(w)
			return
		}
		resume(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, true)
	if !strings.Contains(out.String(), "node n1 paused") {
		t.Errorf("stdout = %q; want the cut run's pause line", out.String())
	}
	assertFollowedThrough(t, code, out.String(), errOut.String())
}

// A POST stream cut mid-run hands off to the GET stream past its last id instead of reporting a partial answer.
func TestRunChatSendResumesAfterDrop(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	shortSSEReconnect(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/c1/responses", func(w http.ResponseWriter, _ *http.Request) { cutRun(w) })
	mux.HandleFunc("GET /api/v1/chats/c1/stream", restartedStream(t))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	assertFollowedThrough(t, code, out.String(), errOut.String())
}

// A POST the server accepted (200) but cut before its first event still follows the run from the start, instead of
// reporting an empty answer with exit 0.
func TestRunChatSendFollowsAPostCutBeforeItsFirstEvent(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	shortSSEReconnect(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/c1/responses", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // accepted, then a graceful stop ends the body with no event
	})
	mux.HandleFunc("GET /api/v1/chats/c1/stream", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != "" {
			t.Errorf("Last-Event-ID = %q, want none: nothing was seen yet", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, 1, "node_start", `{"node_id":"n1"}`)
		writeSSE(w, 2, "node_done", `{"node_id":"n1","output":"final answer"}`)
		writeSSE(w, 3, "done", `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	assertFollowedThrough(t, code, out.String(), errOut.String())
}

// onEvent's own error stops the stream, per SendMessage's contract; it must not read as a drop to resume.
func TestSendMessageCallerStopIsNoDrop(t *testing.T) {
	shortSSEReconnect(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/c1/responses", func(w http.ResponseWriter, _ *http.Request) { cutRun(w) })
	mux.HandleFunc("GET /api/v1/chats/c1/stream", func(http.ResponseWriter, *http.Request) {
		t.Error("a caller's stop resumed the stream")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stop := errors.New("enough")
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.SendMessage(context.Background(), "c1", "hi", func(SSEEvent) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("SendMessage = %v; want the caller's stop error", err)
	}
}

// The reconnect budget must outlast a deploy's restart (~50s); the web UI allows ~3 minutes.
func TestSSEReconnectBudgetOutlastsADeploy(t *testing.T) {
	var total time.Duration
	for i := range maxSSEReconnectAttempts {
		total += sseReconnectDelay(i)
	}
	if total < 150*time.Second || total > 4*time.Minute {
		t.Fatalf("reconnect budget = %s, want ~3 minutes", total)
	}
}

// Each delivered event resets the budget, so a long run surviving several drops isn't cut off by their sum.
func TestSubscribeSSEBudgetResetsOnProgress(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	shortSSEReconnect(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, i, "agent_token", `{"text":"x"}`)
		if i == maxSSEReconnectAttempts+3 {
			io.WriteString(w, "event: done\ndata: {}\n\n")
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.subscribeSSE(context.Background(), "c1", "", func(SSEEvent) error { return nil }); err != nil {
		t.Fatalf("subscribeSSE: %v; want each drop after progress to start a fresh budget", err)
	}
}
