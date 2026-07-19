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

// chatShowDetailJSON is a chat with a two-node DAG (model + token + duration +
// score data on one node, a bare failed node on the other) so the test covers
// the plan's "node table with model + token columns" case (test case 5).
const chatShowDetailJSON = `{
  "id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
  "system_prompt":"","title":"Research run","status":"needs_input","pending_question":"which region?",
  "turns":[{"id":"t1","created_at":"2026-01-01T00:00:00Z",
    "input":{"role":"user","content":"research it"},
    "output":[
      {"type":"quack:dag","id":"d1","status":"in_progress","plan_id":"p1",
       "nodes":[{"id":"n1","agent":"web-researcher","task":"t","depends_on":[]},
                {"id":"n2","agent":"synthesizer","task":"t","depends_on":["n1"]}],
       "edges":[{"from":"n1","to":"n2"}],
       "node_states":{
         "n1":{"status":"done","model":"qwen3.6-35b","total_tokens":1234,"server_duration_ms":2500,"judge_final_score":0.82},
         "n2":{"status":"failed"}
       }},
      {"type":"message","id":"m1","status":"completed","content":[{"type":"output_text","text":"partial answer"}]}
    ]}]
}`

func TestRunChatShow(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chats/c1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, chatShowDetailJSON)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (snapshot mode never fails on its own)", code)
	}
	s := out.String()
	for _, want := range []string{
		"id:     c1", "title:  Research run", "status: needs_input", "question: which region?",
		"NODE", "AGENT", "STATUS", "MODEL", "TOKENS", "DURATION", "SCORE",
		"n1", "web-researcher", "done", "qwen3.6-35b", "1234", "2.5s", "0.82",
		"n2", "synthesizer", "failed",
		"partial answer",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("chat show output missing %q:\n%s", want, s)
		}
	}
}

// chatShowReasoningLeakJSON pins #419: a message item whose content mixes a
// reasoning part ahead of the output_text part — ReasoningPart and
// OutputTextPart share the same {text,type} JSON shape, so a naive "does it
// unmarshal" check on AsOutputTextPart() would let the raw thinking through.
const chatShowReasoningLeakJSON = `{
  "id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
  "system_prompt":"","title":"Plan run","status":"completed",
  "turns":[{"id":"t1","created_at":"2026-01-01T00:00:00Z",
    "input":{"role":"user","content":"plan it"},
    "output":[
      {"type":"message","id":"m1","status":"completed","content":[
        {"type":"reasoning","text":"The user wants me to produce an implementation plan... let me start by loading the relevant skills..."},
        {"type":"output_text","text":"Here is the plan."}
      ]}
    ]}]
}`

// TestRunChatShowOmitsReasoning pins #419: the non-follow snapshot must not
// leak raw orchestrator thinking into the printed answer.
func TestRunChatShowOmitsReasoning(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, chatShowReasoningLeakJSON)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, "Here is the plan.") {
		t.Errorf("chat show output missing the answer text:\n%s", s)
	}
	if strings.Contains(s, "let me start by loading") {
		t.Errorf("chat show output leaked raw reasoning text:\n%s", s)
	}
}

// TestRunChatShowGithubLink pins #382: `chat show` surfaces the originating
// GitHub PR/issue link when the chat carries one.
func TestRunChatShowGithubLink(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	const withGithub = `{
	  "id":"github-acme-widgets-7","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
	  "system_prompt":"","title":"Widgets leak memory","status":"completed",
	  "github_repo":"acme/widgets","github_url":"https://github.com/acme/widgets/issues/7",
	  "turns":[]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, withGithub)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "github-acme-widgets-7", false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "github: https://github.com/acme/widgets/issues/7") {
		t.Errorf("chat show output missing the github link:\n%s", out.String())
	}
}

// TestRunChatShowNoGithubLink pins the negative case: a direct (non-github)
// chat shows nothing extra — no "github:" line at all.
func TestRunChatShowNoGithubLink(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, chatShowDetailJSON)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "github:") {
		t.Errorf("direct chat should show no github line:\n%s", out.String())
	}
}

func TestRunChatShowJSON(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, chatShowDetailJSON)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", true, false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var detail schema.ChatDetail
	if err := json.Unmarshal(out.Bytes(), &detail); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
	}
	if detail.Id != "c1" || detail.Status != schema.ChatStatusNeedsInput {
		t.Errorf("decoded detail = %+v, want id c1 status needs_input", detail)
	}
}

func TestRunChatShowNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "nope", false, false)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "nope not found") {
		t.Errorf("stderr = %q, want a chat-not-found message", errOut.String())
	}
}

// TestRunChatShowFollowNotRunning: -f on an idle chat is a no-op with a note,
// not an attempt to attach to a dead stream.
func TestRunChatShowFollowNotRunning(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","turns":[]}`)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "nothing running") {
		t.Errorf("output = %q, want a nothing-running note", out.String())
	}
}

// TestRunChatShowFollowLive: -f on a running chat prints the snapshot, then
// line-oriented events from Subscribe until the run ends, applying the same
// pause semantics as `chat send` (needs_input → exit 2).
func TestRunChatShowFollowLive(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("follow: method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: node_start\ndata: {\"node_id\":\"n1\"}\n\n")
		io.WriteString(w, "event: node_needs_input\ndata: {\"node_id\":\"n1\",\"message\":\"which region?\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, true)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	s := out.String()
	if !strings.Contains(s, "node n1 running") || !strings.Contains(s, "node n1 needs_input: which region?") {
		t.Errorf("follow output = %q, want line-oriented node events", s)
	}
	if !strings.Contains(s, "question: which region?") {
		t.Errorf("follow output = %q, want the final question: line", s)
	}
}

// TestRunChatShowFollowToolsAndThinking pins #385's CLI half: `chat show -f`
// used to have no case at all for agent_thinking/agent_tool_call/
// agent_tool_result — tool calls and reasoning were invisible in the
// terminal. It now renders a terse, one-line-per-event trace: "thinking…"
// once per reasoning block (not once per streamed delta), and a "tool: …" /
// "→ …" pair per call — never a raw JSON dump.
func TestRunChatShowFollowToolsAndThinking(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Reasoning streams as several small deltas — must collapse to ONE line.
		io.WriteString(w, "event: agent_thinking\ndata: {\"node_id\":\"n1\",\"run_id\":\"r1\",\"text\":\"I should \"}\n\n")
		io.WriteString(w, "event: agent_thinking\ndata: {\"node_id\":\"n1\",\"run_id\":\"r1\",\"text\":\"check the tests\"}\n\n")
		io.WriteString(w, "event: agent_tool_call\ndata: {\"node_id\":\"n1\",\"run_id\":\"r1\",\"call_id\":\"c1\",\"name\":\"run_command\",\"args\":{\"command\":\"go test ./...\"}}\n\n")
		io.WriteString(w, "event: agent_tool_result\ndata: {\"node_id\":\"n1\",\"run_id\":\"r1\",\"call_id\":\"c1\",\"name\":\"run_command\",\"result\":{\"exit_code\":0}}\n\n")
		io.WriteString(w, "event: node_done\ndata: {\"node_id\":\"n1\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	s := out.String()
	if got := strings.Count(s, "thinking…"); got != 1 {
		t.Errorf("thinking… lines = %d, want exactly 1 (deltas of the same run must collapse), output:\n%s", got, s)
	}
	if !strings.Contains(s, `node n1: tool: run_command("go test ./...")`) {
		t.Errorf("follow output missing the compact tool-call line:\n%s", s)
	}
	if !strings.Contains(s, "node n1:   → exit 0") {
		t.Errorf("follow output missing the compact tool-result line:\n%s", s)
	}
	if strings.Contains(s, `"exit_code":0`) {
		t.Errorf("follow output must not dump the raw tool result JSON:\n%s", s)
	}
}

// TestRunChatShowFollowDiscardsPreamble pins #387 in the CLI: the old
// per-token live print showed narration ahead of a tool call as if it were
// already the answer, with no way to "un-print" it once a later tool call
// proved it wasn't. `-f` no longer streams top-level tokens live at all; the
// corrected (preamble-free) answer prints once, at the end, via the same
// Report() path `chat send` uses.
func TestRunChatShowFollowDiscardsPreamble(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"Let me look into that.\"}\n\n")
		io.WriteString(w, "event: agent_tool_call\ndata: {\"call_id\":\"c1\",\"name\":\"get_user_choice\",\"args\":{}}\n\n")
		io.WriteString(w, "event: agent_tool_result\ndata: {\"call_id\":\"c1\",\"name\":\"get_user_choice\",\"result\":{}}\n\n")
		io.WriteString(w, "event: agent_token\ndata: {\"text\":\"the real answer\"}\n\n")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	s := out.String()
	if strings.Contains(s, "Let me look into that.") {
		t.Errorf("follow output must not print pre-tool-call narration:\n%s", s)
	}
	if !strings.Contains(s, "the real answer") {
		t.Errorf("follow output missing the final answer:\n%s", s)
	}
}
