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
	if err := PrintPrompt(context.Background(), &out, nil, srv.URL, "hi"); err != nil {
		t.Fatalf("PrintPrompt: %v", err)
	}
	if got := out.String(); got != "Hello world\n" {
		t.Errorf("output = %q, want %q", got, "Hello world\n")
	}
}

// TestRunAPI covers the raw passthrough: GET prints the body (+ trailing
// newline), POST forwards the request body, and a 4xx returns an error while
// still printing the response body.
func TestRunAPI(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || string(b) != `{"x":1}` {
			t.Errorf("POST body = %q method = %s", b, r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"c1"}`)
	})
	mux.HandleFunc("/nope", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "not found")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	if err := RunAPI(context.Background(), &out, srv.URL, "GET", "/health", nil); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if out.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("GET out = %q", out.String())
	}

	out.Reset()
	if err := RunAPI(context.Background(), &out, srv.URL, "post", "/api/v1/chats", strings.NewReader(`{"x":1}`)); err != nil {
		t.Fatalf("POST: %v", err) // lowercase method is upper-cased by Request
	}

	out.Reset()
	err := RunAPI(context.Background(), &out, srv.URL, "GET", "/nope", nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404 error", err)
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("body should print even on error: %q", out.String())
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
	err := PrintPrompt(context.Background(), &out, nil, srv.URL, "hi")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to contain the server error", err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed on error, got %q", out.String())
	}
}

// TestSteerNode posts the guidance to the node steer endpoint and accepts the
// 204 the server returns (postJSON's 200-only check would reject it).
func TestSteerNode(t *testing.T) {
	var gotBody schema.SteerNodeBody
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/nodes/n2/steer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.SteerNode(context.Background(), "c1", "n2", "focus on cost"); err != nil {
		t.Fatalf("SteerNode: %v", err)
	}
	if gotBody.Guidance != "focus on cost" {
		t.Errorf("server got guidance %q, want %q", gotBody.Guidance, "focus on cost")
	}
}
