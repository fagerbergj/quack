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
)

// TestSubscribe drives the resume transport: a GET to the chat's stream endpoint
// whose SSE events arrive on the channel in order, closing on stream end.
func TestSubscribe(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("subscribe: method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: node_start\ndata: {\"node_id\":\"a\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	var names []string
	for ev := range c.Subscribe(context.Background(), "c1") {
		names = append(names, ev.Name)
	}
	if got := strings.Join(names, ","); got != "node_start,done" {
		t.Errorf("events = %q, want node_start,done", got)
	}
}

// chatDetailJSON is a hand-written ChatDetail so the test exercises the real
// generated-union parsing (message output item → text) the export path relies on.
const chatDetailJSON = `{
  "id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
  "system_prompt":"","title":"Greeting","status":"idle",
  "turns":[{"id":"t1","created_at":"2026-01-01T00:00:00Z",
    "input":{"role":"user","content":"hi there"},
    "output":[
      {"type":"quack:dag","id":"d1","status":"completed","plan_id":"p1","nodes":[],"edges":[],"node_states":[]},
      {"type":"message","id":"m1","status":"completed","content":[{"type":"output_text","text":"hello back"}]}
    ]}]
}`

func TestRunChatList(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/chats" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"First","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T03:04:00Z","system_prompt":"","status":"idle"},
			{"id":"c2","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"ID", "TITLE", "STATUS", "c1", "First", "c2", "(untitled)"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q:\n%s", want, s)
		}
	}
}

// TestRunChatListStatuses covers the plan's test case 1: STATUS renders for all
// four ChatStatus values and each row is uniquely grep-able by its status (e.g.
// `grep needs_input` matches exactly the c2 row, not c1's "idle" or c4's
// "failed").
func TestRunChatListStatuses(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Idle one","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"c2","title":"Waiting","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"needs_input","pending_question":"which region?"},
			{"id":"c3","title":"Live","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running"},
			{"id":"c4","title":"Broke","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"failed"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	byStatus := map[string]string{}
	for _, status := range []string{"idle", "needs_input", "running", "failed"} {
		var matches []string
		for _, l := range lines {
			if strings.Contains(l, status) {
				matches = append(matches, l)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("grep %q matched %d lines, want exactly 1:\n%s", status, len(matches), out.String())
		}
		byStatus[status] = matches[0]
	}
	if !strings.Contains(byStatus["needs_input"], "c2") {
		t.Errorf("needs_input row = %q, want it to be c2's row", byStatus["needs_input"])
	}
	// The pending question itself is NOT in the list table (chat show/--json's job).
	if strings.Contains(out.String(), "which region?") {
		t.Error("list table should not include the pending question text")
	}
}

func TestRunChatListEmpty(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No chats yet") {
		t.Errorf("empty list should guide the user, got %q", out.String())
	}
}

func TestRunChatExport(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chats/c1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, chatDetailJSON)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatExport(context.Background(), &out, srv.URL, "c1", false); err != nil {
		t.Fatalf("RunChatExport: %v", err)
	}
	s := out.String()
	for _, want := range []string{"# Greeting", "## You", "hi there", "## Duck", "hello back"} {
		if !strings.Contains(s, want) {
			t.Errorf("transcript missing %q:\n%s", want, s)
		}
	}
}

func TestRunChatExportNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	var out bytes.Buffer
	err := RunChatExport(context.Background(), &out, srv.URL, "nope", false)
	if err == nil || !strings.Contains(err.Error(), "nope not found") {
		t.Errorf("err = %v, want a chat-not-found message", err)
	}
}

func TestRunChatDelete(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/chats/c1" {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	// yes=true skips the prompt.
	var out bytes.Buffer
	if err := RunChatDelete(context.Background(), &out, strings.NewReader(""), srv.URL, "c1", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted {
		t.Error("server DELETE was not called")
	}

	// yes=false with a "no" answer must NOT delete.
	deleted = false
	out.Reset()
	if err := RunChatDelete(context.Background(), &out, strings.NewReader("n\n"), srv.URL, "c1", false); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("declining the prompt should not delete")
	}
	if !strings.Contains(out.String(), "Cancelled") {
		t.Errorf("declining should say Cancelled, got %q", out.String())
	}
}

func TestRunNodeStop(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var hit string
	var gotBody schema.NodeStatusUpdateBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v1/chats/c1/nodes/n2/status" {
			hit = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(schema.DagNodeState{Status: schema.NodeStatusCancelled})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunNodeStop(context.Background(), &out, srv.URL, "c1", "n2"); err != nil {
		t.Fatal(err)
	}
	if hit != "/api/v1/chats/c1/nodes/n2/status" {
		t.Errorf("node stop hit %q, want /api/v1/chats/c1/nodes/n2/status", hit)
	}
	if gotBody.Status != schema.NodeStatusCancelled {
		t.Errorf("node stop sent status %q, want %q", gotBody.Status, schema.NodeStatusCancelled)
	}
}

func TestRunChatStop(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var cancelled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/c1":
			_ = json.NewEncoder(w).Encode(schema.ChatDetail{
				Turns: []schema.Turn{{Id: "t1"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/chats/c1/responses/t1/status":
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunChatStop(context.Background(), &out, srv.URL, "c1"); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Error("stop should PUT the response status to cancelled")
	}
}
