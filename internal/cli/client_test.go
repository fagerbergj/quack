package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

// TestPrintPrompt drives the full print-mode path against a fake server: create
// a chat, stream SSE, and prove only top-level agent_token text (no node_id) is
// printed — node-scoped tokens are intermediate research output and excluded.
func TestPrintPrompt(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir()) // isolate from the real registry

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("create chat: method = %s, want POST", r.Method)
		}
		_ = json.NewEncoder(w).Encode(schema.ChatSummary{Id: "c1"})
	})
	mux.HandleFunc("/api/v1/chats/c1/responses", func(w http.ResponseWriter, r *http.Request) {
		var b schema.SendMessageBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Content != "hi" {
			t.Errorf("send: content = %q, want hi", b.Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"Hello \"}\n\n")
		io.WriteString(w, "event: agent_token\ndata: {\"node_id\":\"n1\",\"text\":\"IGNORED\"}\n\n")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"world\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	if err := PrintPrompt(context.Background(), &out, srv.URL, "hi"); err != nil {
		t.Fatalf("PrintPrompt: %v", err)
	}
	if got := out.String(); got != "Hello world\n" {
		t.Errorf("output = %q, want %q", got, "Hello world\n")
	}
}

// TestPrintPromptServerError: an `error` SSE event surfaces as a returned error
// (so the command exits non-zero) and nothing is printed to stdout.
func TestPrintPromptServerError(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(schema.ChatSummary{Id: "c1"})
	})
	mux.HandleFunc("/api/v1/chats/c1/responses", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "event: error\ndata: {\"error\":\"boom\"}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	err := PrintPrompt(context.Background(), &out, srv.URL, "hi")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to contain the server error", err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed on error, got %q", out.String())
	}
}
