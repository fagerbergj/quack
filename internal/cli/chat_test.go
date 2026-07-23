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
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all"}); err != nil {
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
// five ChatStatus values and each row is uniquely grep-able by its status (e.g.
// `grep needs_input` matches exactly the c2 row, not c1's "idle" or c4's
// "failed"). Includes queued (#417): a chat admitted but still waiting on the
// server's max_active_runs slot, distinct from running.
func TestRunChatListStatuses(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Idle one","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"c2","title":"Waiting","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"needs_input","pending_question":"which region?"},
			{"id":"c3","title":"Live","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running"},
			{"id":"c4","title":"Broke","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"failed"},
			{"id":"c5","title":"Behind the cap","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"queued"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all"}); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	byStatus := map[string]string{}
	for _, status := range []string{"idle", "needs_input", "running", "failed", "queued"} {
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
	if !strings.Contains(byStatus["queued"], "c5") {
		t.Errorf("queued row = %q, want it to be c5's row", byStatus["queued"])
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
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No chats yet") {
		t.Errorf("empty list should guide the user, got %q", out.String())
	}
}

// TestRunChatListOrigin covers issue #386: the ORIGIN column reads
// github/direct off the same signal (github_url, falling back to the
// "github-" id prefix) the web ChatList badge uses.
func TestRunChatListOrigin(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Direct","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"github-acme-widget-1","title":"Via issue","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/issues/1","github_repo":"acme/widget"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all"}); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "ORIGIN") {
		t.Errorf("header missing ORIGIN column:\n%s", s)
	}
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(l, "c1"):
			if !strings.Contains(l, "direct") {
				t.Errorf("direct chat row missing origin=direct: %q", l)
			}
		case strings.Contains(l, "github-acme-widget-1"):
			if !strings.Contains(l, "github") {
				t.Errorf("github chat row missing origin=github: %q", l)
			}
		}
	}
}

// TestRunChatListFilter covers the --filter flag: "github" and "direct"
// each narrow to exactly the matching row; an unrecognised value errors.
func TestRunChatListFilter(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Direct","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"github-acme-widget-1","title":"Via issue","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/issues/1"}
		]}`)
	}))
	defer srv.Close()

	var githubOut bytes.Buffer
	if err := RunChatList(context.Background(), &githubOut, srv.URL, false, chatListFilters{origin: "github"}); err != nil {
		t.Fatalf("RunChatList(github): %v", err)
	}
	if strings.Contains(githubOut.String(), "c1") {
		t.Errorf("--filter github should exclude the direct chat: %q", githubOut.String())
	}
	if !strings.Contains(githubOut.String(), "github-acme-widget-1") {
		t.Errorf("--filter github should include the github chat: %q", githubOut.String())
	}

	var directOut bytes.Buffer
	if err := RunChatList(context.Background(), &directOut, srv.URL, false, chatListFilters{origin: "direct"}); err != nil {
		t.Fatalf("RunChatList(direct): %v", err)
	}
	if strings.Contains(directOut.String(), "github-acme-widget-1") {
		t.Errorf("--filter direct should exclude the github chat: %q", directOut.String())
	}
	if !strings.Contains(directOut.String(), "c1") {
		t.Errorf("--filter direct should include the direct chat: %q", directOut.String())
	}

	var errOut bytes.Buffer
	if err := RunChatList(context.Background(), &errOut, srv.URL, false, chatListFilters{origin: "bogus"}); err == nil {
		t.Error("expected an error for an unrecognised --filter value")
	}
}

// TestRunChatListRef covers the REF column: Issue #N / PR #N for
// GitHub-originated chats, "-" for a direct chat.
func TestRunChatListRef(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Direct","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"github-acme-widget-249","title":"Issue chat","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/issues/249","github_repo":"acme/widget"},
			{"id":"github-acme-widget-257","title":"PR chat","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/pull/257","github_repo":"acme/widget"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all"}); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "REF") {
		t.Errorf("header missing REF column:\n%s", s)
	}
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(l, "c1"):
			if !strings.Contains(l, "-") {
				t.Errorf("direct chat row missing ref=-: %q", l)
			}
		case strings.Contains(l, "github-acme-widget-249"):
			if !strings.Contains(l, "Issue #249") {
				t.Errorf("issue chat row missing ref: %q", l)
			}
		case strings.Contains(l, "github-acme-widget-257"):
			if !strings.Contains(l, "PR #257") {
				t.Errorf("pr chat row missing ref: %q", l)
			}
		}
	}
}

// TestRunChatListStatusFilter covers the --status flag: it narrows to the
// exact ChatStatus, alongside (not instead of) --filter.
func TestRunChatListStatusFilter(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Idle","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"c2","title":"Running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all", status: "running"}); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	if strings.Contains(s, "c1") {
		t.Errorf("--status running should exclude the idle chat: %q", s)
	}
	if !strings.Contains(s, "c2") {
		t.Errorf("--status running should include the running chat: %q", s)
	}

	var errOut bytes.Buffer
	if err := RunChatList(context.Background(), &errOut, srv.URL, false, chatListFilters{origin: "all", status: "bogus"}); err == nil {
		t.Error("expected an error for an unrecognised --status value")
	}
}

// TestRunChatListRepoAndTypeFilter covers the --repo and --type flags, and
// that they combine with each other (AND across facets, matching the web
// sidebar's matchesFacets semantics).
func TestRunChatListRepoAndTypeFilter(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"c1","title":"Direct","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle"},
			{"id":"g1","title":"Widget issue","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/issues/1","github_repo":"acme/widget"},
			{"id":"g2","title":"Widget PR","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/widget/pull/2","github_repo":"acme/widget"},
			{"id":"g3","title":"Other repo issue","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"idle","github_url":"https://github.com/acme/other/issues/3","github_repo":"acme/other"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunChatList(context.Background(), &out, srv.URL, false, chatListFilters{origin: "all", repo: "acme/widget", kind: "issue"}); err != nil {
		t.Fatalf("RunChatList: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "g1") {
		t.Errorf("--repo acme/widget --type issue should include g1: %q", s)
	}
	for _, excluded := range []string{"c1", "g2", "g3"} {
		if strings.Contains(s, excluded) {
			t.Errorf("--repo acme/widget --type issue should exclude %s: %q", excluded, s)
		}
	}

	var errOut bytes.Buffer
	if err := RunChatList(context.Background(), &errOut, srv.URL, false, chatListFilters{origin: "all", kind: "bogus"}); err == nil {
		t.Error("expected an error for an unrecognised --type value")
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

// TestRunNodePause PUTs {"status":"paused"} to the node status endpoint and
// confirms on stdout.
func TestRunNodePause(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotBody schema.NodeStatusUpdateBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v1/chats/c1/nodes/n2/status" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(schema.DagNodeState{Status: schema.NodeStatusPaused})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunNodePause(context.Background(), &out, srv.URL, "c1", "n2"); err != nil {
		t.Fatal(err)
	}
	if gotBody.Status != schema.NodeStatusPaused {
		t.Errorf("pause sent status %q, want %q", gotBody.Status, schema.NodeStatusPaused)
	}
	if !strings.Contains(out.String(), "Paused node n2") {
		t.Errorf("pause output %q lacks confirmation", out.String())
	}
}

// TestRunNodeQueue POSTs {"message":...} to the node queue endpoint and
// confirms on stdout with the created message id.
func TestRunNodeQueue(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotBody schema.QueueMessageBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/c1/nodes/n2/queue" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(schema.QueuedMessage{Id: "q1", Text: gotBody.Message})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := RunNodeQueue(context.Background(), &out, srv.URL, "c1", "n2", "focus on cost"); err != nil {
		t.Fatal(err)
	}
	if gotBody.Message != "focus on cost" {
		t.Errorf("queue sent message %q, want %q", gotBody.Message, "focus on cost")
	}
	if !strings.Contains(out.String(), "Queued message q1") {
		t.Errorf("queue output %q lacks confirmation", out.String())
	}
}

// TestRunNodeRetry PUTs {"status":"queued"} (guidance omitted when blank, set
// when given) - updateNodeStatus's retry transition.
func TestRunNodeRetry(t *testing.T) {
	for _, guidance := range []string{"", "use the newer source"} {
		t.Setenv("QUACK_HOME", t.TempDir())
		var gotBody schema.NodeStatusUpdateBody
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && r.URL.Path == "/api/v1/chats/c1/nodes/n2/status" {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				_ = json.NewEncoder(w).Encode(schema.DagNodeState{Status: schema.NodeStatusQueued})
				return
			}
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}))
		var out bytes.Buffer
		if err := RunNodeRetry(context.Background(), &out, srv.URL, "c1", "n2", guidance); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if gotBody.Status != schema.NodeStatusQueued {
			t.Errorf("retry sent status %q, want %q", gotBody.Status, schema.NodeStatusQueued)
		}
		if guidance == "" && gotBody.Guidance != nil {
			t.Errorf("blank guidance should be omitted, got %v", *gotBody.Guidance)
		}
		if guidance != "" && (gotBody.Guidance == nil || *gotBody.Guidance != guidance) {
			t.Errorf("retry sent guidance %v, want %q", gotBody.Guidance, guidance)
		}
	}
}

// TestPutStatusSurfaces409Reason: an illegal transition's TransitionError body
// (error + allowed statuses) reaches the user, not a bare "409 Conflict".
func TestPutStatusSurfaces409Reason(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "node is done", "current": "done", "allowed": []string{"queued"},
		})
	}))
	defer srv.Close()
	err := RunNodePause(context.Background(), io.Discard, srv.URL, "c1", "n2")
	if err == nil {
		t.Fatal("expected an error on 409")
	}
	for _, want := range []string{"node is done", "allowed: queued"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("409 error %q should contain %q", err.Error(), want)
		}
	}
}
