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

// chatShowDetailJSON is a two-node DAG: one node with model, token, duration and score data,
// one bare failed node.
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
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (the fixture's status is needs_input)", code)
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

// chatShowReasoningLeakJSON mixes a reasoning part ahead of output_text; both share a {text,type}
// shape, so a naive AsOutputTextPart() check would leak the thinking.
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

// TestRunChatShowOmitsReasoning: the non-follow snapshot must not
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

// TestRunChatShowGithubLink: `chat show` surfaces the originating
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
// chat shows nothing extra - no "github:" line at all.
func TestRunChatShowNoGithubLink(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, chatShowDetailJSON)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, false)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (the fixture's status is needs_input)", code)
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
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (the fixture's status is needs_input)", code)
	}
	var detail schema.ChatDetail
	if err := json.Unmarshal(out.Bytes(), &detail); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
	}
	if detail.Id != "c1" || detail.Status != schema.ChatStatusNeedsInput {
		t.Errorf("decoded detail = %+v, want id c1 status needs_input", detail)
	}
}

// Both paths must apply the 0/1/2 exit-code contract off the chat's status.
func TestRunChatShowExitCodeContract(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"idle", 0},
		{"failed", 1},
		{"needs_input", 2},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(tc.status, func(t *testing.T) {
				t.Setenv("QUACK_HOME", t.TempDir())
				body := `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",` +
					`"system_prompt":"","status":"` + tc.status + `","turns":[]}`
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					io.WriteString(w, body)
				}))
				defer srv.Close()

				var out, errOut bytes.Buffer
				code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", asJSON, false)
				if code != tc.want {
					t.Errorf("status=%s asJSON=%v: exit code = %d, want %d", tc.status, asJSON, code, tc.want)
				}
			})
		}
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

// TestRunChatShowFollowLive: -f prints the snapshot, then Subscribe events until the run ends,
// with `chat send`'s pause semantics (needs_input exits 2).
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

// TestRunChatShowFollowToolsAndThinking: -f prints one "thinking…" per reasoning block and a
// "tool: …" / "→ …" pair per call, never raw JSON.
func TestRunChatShowFollowToolsAndThinking(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/c1", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/c1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Reasoning streams as several small deltas - must collapse to ONE line.
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

// TestRunChatShowFollowDiscardsPreamble: -f doesn't stream top-level tokens; the preamble-free answer
// prints once at the end via Report.
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

// TestRunChatShowMultiSinkAnswer: a two-sink turn's answer prints every labelled section, not the first sink.
func TestRunChatShowMultiSinkAnswer(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	const detail = `{
  "id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",
  "system_prompt":"","title":"Two researchers","status":"completed",
  "turns":[{"id":"t1","created_at":"2026-01-01T00:00:00Z","input":{"role":"user","content":"two researchers, no synthesizer"},
    "output":[
      {"type":"quack:dag","id":"d1","status":"completed","plan_id":"p1",
       "nodes":[{"id":"r1","agent":"web-researcher","task":"a","depends_on":[]},{"id":"r2","agent":"web-researcher","task":"b","depends_on":[]}],
       "edges":[],"node_states":{"r1":{"status":"done"},"r2":{"status":"done"}}},
      {"type":"message","id":"m1","status":"completed","content":[{"type":"output_text","text":"## r1\n\nONE\n\n## r2\n\nTWO"}]}
    ]}]
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, detail) }))
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := RunChatShow(context.Background(), &out, &errOut, srv.URL, "c1", false, false); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "## r1\n\nONE\n\n## r2\n\nTWO") {
		t.Errorf("chat show output lacks both sections:\n%s", out.String())
	}
}

// TestStreamStateMultiSinkAnswer: `chat send` answers a two-sink plan with both sections, a stopped
// sink masked, and falls back to the last sink's output when every other sink produced nothing.
func TestStreamStateMultiSinkAnswer(t *testing.T) {
	run := func(events ...SSEEvent) string {
		s := newStreamState()
		s.handle(SSEEvent{Name: "dag_plan", Data: json.RawMessage(`{"nodes":[{"id":"r1"},{"id":"r2"}],"edges":[]}`)}, nil)
		for _, ev := range events {
			s.handle(ev, nil)
		}
		return s.result("c1").Answer
	}
	done := func(id, out string) SSEEvent {
		return SSEEvent{Name: "node_done", Data: json.RawMessage(`{"node_id":"` + id + `","output":"` + out + `"}`)}
	}
	cancelled := SSEEvent{Name: "node_cancelled", Data: json.RawMessage(`{"node_id":"r1"}`)}
	if got := run(done("r2", "TWO"), done("r1", "ONE")); got != "## r1\n\nONE\n\n## r2\n\nTWO" {
		t.Errorf("both done: %q", got)
	}
	if got, want := run(cancelled, done("r2", "TWO")), "## r1\n\n_Stopped, not reviewed._\n\n## r2\n\nTWO"; got != want {
		t.Errorf("one stopped: %q, want %q", got, want)
	}
	if got := run(done("r2", "TWO")); got != "TWO" {
		t.Errorf("one sink: %q", got)
	}
}
