package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	var out, errOut bytes.Buffer
	if code := PrintPrompt(context.Background(), &out, &errOut, nil, srv.URL, "hi", nil, false); code != 0 {
		t.Fatalf("PrintPrompt exit = %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := out.String(); got != "Hello world\n" {
		t.Errorf("output = %q, want %q", got, "Hello world\n")
	}
	if got := errOut.String(); got != "chat: c1\n" {
		t.Errorf("stderr = %q, want %q (chat id, separately capturable from the answer)", got, "chat: c1\n")
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

// TestPrintPromptServerError: an `error` SSE event is a failure — exit 1, the
// message on stderr, nothing on stdout (test case 6 in the PR spec).
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

	var out, errOut bytes.Buffer
	code := PrintPrompt(context.Background(), &out, &errOut, nil, srv.URL, "hi", nil, false)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "boom") {
		t.Errorf("stderr = %q, want it to contain the server error", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed to stdout on failure, got %q", out.String())
	}
}

// TestPrintPromptNeedsInput: a paused run (node_needs_input) prints `question:
// <text>` on stdout, a hint on stderr, and exits 2 — --json mode reports the
// same status/exit code via one JSON object.
func TestPrintPromptNeedsInput(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(schema.ChatSummary{Id: "c1"})
	})
	mux.HandleFunc("/api/v1/chats/c1/responses", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "event: node_needs_input\ndata: {\"node_id\":\"n1\",\"message\":\"which region?\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := PrintPrompt(context.Background(), &out, &errOut, nil, srv.URL, "hi", nil, false)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(out.String()); got != "question: which region?" {
		t.Errorf("stdout = %q, want %q", got, "question: which region?")
	}
	if !strings.Contains(errOut.String(), "quack chat send c1") {
		t.Errorf("stderr hint = %q, want it to name `chat send c1`", errOut.String())
	}

	// --json: same status/exit code, one object.
	out.Reset()
	errOut.Reset()
	code = PrintPrompt(context.Background(), &out, &errOut, nil, srv.URL, "hi", nil, true)
	if code != 2 {
		t.Errorf("--json exit code = %d, want 2", code)
	}
	var res SendResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
	}
	if res.Status != StatusNeedsInput || res.Question != "which region?" {
		t.Errorf("json result = %+v, want status needs_input, question %q", res, "which region?")
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

func TestSendMessageWithFiles(t *testing.T) {
	var gotContent, gotName, gotCT string
	var gotData []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		gotContent = r.FormValue("content")
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				gotName, gotCT = fh.Filename, fh.Header.Get("Content-Type")
				f, _ := fh.Open()
				gotData, _ = io.ReadAll(f)
				f.Close()
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer srv.Close()

	img := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(img, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.SendMessageWithFiles(context.Background(), "chat1", "hi", []string{img}, func(SSEEvent) error { return nil }); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotContent != "hi" {
		t.Errorf("content = %q, want hi", gotContent)
	}
	if gotName != "pic.png" {
		t.Errorf("filename = %q, want pic.png", gotName)
	}
	if gotCT != "image/png" { // inferred from extension
		t.Errorf("file content-type = %q, want image/png", gotCT)
	}
	if len(gotData) != 3 {
		t.Errorf("file bytes = %d, want 3", len(gotData))
	}
}
