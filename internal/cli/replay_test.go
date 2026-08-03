package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRunReplay_CompletedRun: create-chat + send-prompt + stream, exactly
// like PrintPrompt's flow, but talking to base DIRECTLY (no LoadClient/
// registry involved - the caller already resolved which server to hit).
func TestRunReplay_CompletedRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"replay-c1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/replay-c1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: node_start\ndata: {\"node_id\":\"n1\"}\n\n")
			io.WriteString(w, "event: node_done\ndata: {\"node_id\":\"n1\",\"output\":\"the replayed answer\"}\n\n")
			io.WriteString(w, "event: done\ndata: {}\n\n")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunReplay(context.Background(), &out, &errOut, srv.URL, "do the recorded task", false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); !strings.Contains(got, "the replayed answer") {
		t.Errorf("stdout = %q, want it to contain the final answer", got)
	}
	if !strings.Contains(errOut.String(), "chat: replay-c1") {
		t.Errorf("stderr = %q, want the created chat id announced", errOut.String())
	}
}

// TestRunReplay_CreateChatFails: a create-chat failure (server unreachable,
// or a 5xx) is reported and exits 1 - never a panic on a nil chat id.
func TestRunReplay_CreateChatFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunReplay(context.Background(), &out, &errOut, srv.URL, "prompt", false)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if errOut.Len() == 0 {
		t.Error("want an error message on stderr")
	}
}
