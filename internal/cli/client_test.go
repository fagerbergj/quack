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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/schema"
)

// TestPrintPrompt drives the full print-mode path against a fake server: create
// a chat, stream SSE, and prove only top-level agent_token text (no node_id) is
// printed - node-scoped tokens are intermediate research output and excluded.
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

// TestPrintPromptServerError: an `error` SSE event is a failure - exit 1, the
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
// <text>` on stdout, a hint on stderr, and exits 2 - --json mode reports the
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

// TestPauseNode PUTs {"status":"paused"} to the node status endpoint and
// accepts the 200 the server returns.
func TestPauseNode(t *testing.T) {
	var gotBody schema.NodeStatusUpdateBody
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/nodes/n2/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.DagNodeState{Status: schema.NodeStatusPaused})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.PauseNode(context.Background(), "c1", "n2"); err != nil {
		t.Fatalf("PauseNode: %v", err)
	}
	if gotBody.Status != schema.NodeStatusPaused {
		t.Errorf("server got status %q, want %q", gotBody.Status, schema.NodeStatusPaused)
	}
}

// TestResumeNode PUTs {"status":"running"} (no guidance) to the node status
// endpoint.
func TestResumeNode(t *testing.T) {
	var gotBody schema.NodeStatusUpdateBody
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/nodes/n2/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.DagNodeState{Status: schema.NodeStatusQueued})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.ResumeNode(context.Background(), "c1", "n2"); err != nil {
		t.Fatalf("ResumeNode: %v", err)
	}
	if gotBody.Status != schema.NodeStatusRunning {
		t.Errorf("server got status %q, want %q", gotBody.Status, schema.NodeStatusRunning)
	}
	if gotBody.Guidance != nil {
		t.Errorf("server got guidance %v, want nil (resume carries no guidance)", gotBody.Guidance)
	}
}

// TestQueueNodeMessage POSTs {"message":...} to the node queue endpoint and
// decodes the created QueuedMessage.
func TestQueueNodeMessage(t *testing.T) {
	var gotBody schema.QueueMessageBody
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/nodes/n2/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.QueuedMessage{Id: "q1", Text: gotBody.Message, Delivered: false, CreatedAt: time.Now()})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	m, err := c.QueueNodeMessage(context.Background(), "c1", "n2", "focus on cost")
	if err != nil {
		t.Fatalf("QueueNodeMessage: %v", err)
	}
	if m.Id != "q1" || m.Text != "focus on cost" {
		t.Errorf("got %+v, want id=q1 text=%q", m, "focus on cost")
	}
}

// TestEditNodeTask PATCHes the node with {"task":...}; a 409 (already
// started) is surfaced as an error.
func TestEditNodeTask(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/nodes/n2", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the node has already started; its prompt is immutable"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	err := c.EditNodeTask(context.Background(), "c1", "n2", "revised task")
	if err == nil || !strings.Contains(err.Error(), "already started") {
		t.Errorf("EditNodeTask error = %v, want it to mention the node already started", err)
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

// TestSubscribeSSEReconnectsWithLastEventID: issue #383 - a dropped
// subscribe stream (the body closes mid-run, no `done` seen) is retried
// automatically, resuming past the last event actually delivered via
// Last-Event-ID, without losing or duplicating any event.
func TestSubscribeSSEReconnectsWithLastEventID(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	orig := sseReconnectDelay
	sseReconnectDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { sseReconnectDelay = orig })

	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if n == 1 {
			if got := r.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("first request: Last-Event-ID = %q, want none", got)
			}
			io.WriteString(w, "id: 1\nevent: node_start\ndata: {\"node_id\":\"n1\"}\n\n")
			io.WriteString(w, "id: 2\nevent: agent_token\ndata: {\"text\":\"partial\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return // connection drops mid-run - no `done` event
		}
		if got := r.Header.Get("Last-Event-ID"); got != "2" {
			t.Errorf("reconnect: Last-Event-ID = %q, want 2 (the last event actually delivered)", got)
		}
		io.WriteString(w, "id: 3\nevent: agent_token\ndata: {\"text\":\" more\"}\n\n")
		io.WriteString(w, "id: 4\nevent: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	var names []string
	err := c.subscribeSSE(context.Background(), "c1", func(ev SSEEvent) error {
		names = append(names, ev.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribeSSE: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one drop, one successful reconnect)", got)
	}
	want := []string{"node_start", "agent_token", "agent_token", "done"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("events = %v, want %v (no event lost or duplicated across the reconnect)", names, want)
	}
}

// TestSubscribeSSEGivesUpAfterRepeatedDrops: a permanently dead server
// doesn't retry forever - it surfaces an error after the bounded attempt cap.
func TestSubscribeSSEGivesUpAfterRepeatedDrops(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	orig := sseReconnectDelay
	sseReconnectDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { sseReconnectDelay = orig })

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Every attempt drops immediately - no events, no `done`.
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	err := c.subscribeSSE(context.Background(), "c1", func(SSEEvent) error { return nil })
	if err == nil {
		t.Fatal("subscribeSSE: want an error after repeated drops, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != maxSSEReconnectAttempts+1 {
		t.Fatalf("attempts = %d, want %d (bounded, not retried forever)", got, maxSSEReconnectAttempts+1)
	}
}
