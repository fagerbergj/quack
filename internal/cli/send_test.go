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

// TestRunChatSendCompleted: a full run streams only the final answer to
// stdout — no trace, no ANSI, exit 0 (plan test case 3).
func TestRunChatSendCompleted(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chats/c1/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"the answer\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "the answer" {
		t.Errorf("stdout = %q, want %q", got, "the answer")
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr should be empty (no --events), got %q", errOut.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("output should contain no ANSI escapes")
	}
}

// TestRunChatSendNeedsInput: a paused run (node_needs_input) prints `question:
// <text>` on stdout + a hint on stderr, exit 2; --json reports the same via
// one object (plan test case 2).
func TestRunChatSendNeedsInput(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: node_needs_input\ndata: {\"node_id\":\"n1\",\"message\":\"which region?\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(out.String()); got != "question: which region?" {
		t.Errorf("stdout = %q, want %q", got, "question: which region?")
	}
	if !strings.Contains(errOut.String(), "answer with: quack chat send c1") {
		t.Errorf("stderr hint = %q, want the chat send hint", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, true)
	if code != 2 {
		t.Errorf("--json exit code = %d, want 2", code)
	}
	var res SendResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
	}
	if res.Status != StatusNeedsInput || res.Question != "which region?" || res.ChatID != "c1" {
		t.Errorf("json result = %+v", res)
	}
}

// TestRunChatSendFailed: a stream error is a failure — exit 1, message on
// stderr, empty stdout (plan test case 6).
func TestRunChatSendFailed(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: error\ndata: {\"error\":\"boom\"}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on failure, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "boom") {
		t.Errorf("stderr = %q, want it to contain the server error", errOut.String())
	}
}

// TestRunChatSendCompleted_DiscardsPreamble: narration the orchestrator emits
// before a top-level tool call ("I'll check the plan...") must not survive
// into the final printed answer — only text after the last tool call does
// (#387, mirrors internal/acp/translate.go's per-round reset, #358).
func TestRunChatSendCompleted_DiscardsPreamble(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"Let me check something first.\"}\n\n")
		io.WriteString(w, "event: agent_tool_call\ndata: {\"call_id\":\"c1\",\"name\":\"get_user_choice\",\"args\":{}}\n\n")
		io.WriteString(w, "event: agent_tool_result\ndata: {\"call_id\":\"c1\",\"name\":\"get_user_choice\",\"result\":{}}\n\n")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"the real answer\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "the real answer" {
		t.Errorf("stdout = %q, want %q (preamble before the tool call must be discarded)", got, "the real answer")
	}
}

// TestRunChatSendEvents: --events routes the pipeline trace to stderr, leaving
// stdout answer-only.
func TestRunChatSendEvents(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"ok\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatSend(context.Background(), &out, &errOut, srv.URL, "c1", "hi", nil, true, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "ok" {
		t.Errorf("stdout = %q, want %q", got, "ok")
	}
	if !strings.Contains(errOut.String(), "agent_token") {
		t.Errorf("stderr trace missing agent_token, got %q", errOut.String())
	}
}
