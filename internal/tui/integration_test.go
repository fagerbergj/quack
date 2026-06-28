package tui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/fagerbergj/quack/internal/cli"
)

func writeSSE(w http.ResponseWriter, name, data string) {
	io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestTUI_RendersRun is the tier-2 integration test: the assembled program drives
// a real (canned) SSE stream through the real client and renders the full
// vocabulary — title, a DAG with per-node status, and the streamed answer. We
// assert on the accumulated output rather than a golden snapshot because the
// alt-screen teardown clobbers FinalOutput (the screen is cleared on exit).
func TestTUI_RendersRun(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/chats" {
			io.WriteString(w, `{"id":"c1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":""}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, "chat_title", `{"title":"Rubber ducks"}`)
		writeSSE(w, "dag_plan", `{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"search","depends_on":[]},{"id":"b","agent":"synthesizer","task":"combine","depends_on":["a"]}],"edges":[]}`)
		writeSSE(w, "node_start", `{"node_id":"a","agent":"researcher"}`)
		writeSSE(w, "node_done", `{"node_id":"a"}`)
		writeSSE(w, "node_start", `{"node_id":"b","agent":"synthesizer"}`)
		writeSSE(w, "node_done", `{"node_id":"b"}`)
		writeSSE(w, "agent_token", `{"text":"Rubber ducks are great."}`)
		writeSSE(w, "done", `{}`)
	}))
	defer srv.Close()

	c, err := cli.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), c, "c1", "", nil, "tell me about ducks", "")
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	// The final frame (after all events) carries the title, the DAG with both
	// nodes done, and the answer — all present in the accumulated output.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("Rubber ducks are great.")) &&
			bytes.Contains(b, []byte("Rubber ducks")) && // title
			bytes.Contains(b, []byte("researcher")) &&
			bytes.Contains(b, []byte("synthesizer")) &&
			bytes.Contains(b, []byte("2/2 done"))
	}, teatest.WithDuration(5*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC}) // idle now → quit
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
