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

// chatDetailJSON is a hand-written ChatDetail so the test exercises the real
// generated-union parsing (message output item → text) the export path relies on.
const chatDetailJSON = `{
  "id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
  "system_prompt":"","title":"Greeting",
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
			{"id":"c1","title":"First","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T03:04:00Z","system_prompt":""},
			{"id":"c2","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":""}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"ID", "TITLE", "c1", "First", "c2", "(untitled)"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q:\n%s", want, s)
		}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			hit = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunNodeStop(context.Background(), &out, srv.URL, "c1", "n2"); err != nil {
		t.Fatal(err)
	}
	if hit != "/api/v1/chats/c1/nodes/n2" {
		t.Errorf("node stop hit %q, want /api/v1/chats/c1/nodes/n2", hit)
	}
}

func TestRunChatStop(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var cancelled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/chats/c1/stream" {
			cancelled = true
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunChatStop(context.Background(), &out, srv.URL, "c1"); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Error("stop should DELETE the stream")
	}
}
