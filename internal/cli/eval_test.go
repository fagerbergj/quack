package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/replay"
)

// newRecordingBundle builds a minimal valid recording ZIP (manifest.json +
// entries.jsonl with one eval.score entry) - the shape FetchRecording
// downloads and replay.Load reads.
func newRecordingBundle(t *testing.T, criterion string, score float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mf, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatalf("create manifest.json: %v", err)
	}
	io.WriteString(mf, `{"session_id":"eval-c1","ledger_version":2}`)

	ef, err := zw.Create("entries.jsonl")
	if err != nil {
		t.Fatalf("create entries.jsonl: %v", err)
	}
	line := `{"seq":1,"chat_id":"eval-c1","kind":"eval.score","at":"2026-01-01T00:00:00Z","payload":{"criterion":"` + criterion + `","score":` +
		strconv.FormatFloat(score, 'f', -1, 64) + `,"response_id":"judge-r1"}}` + "\n"
	io.WriteString(ef, line)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// TestRunEval_MultiTurnAndScored: two recorded turns are sent in order, each
// only after the previous turn's run completes; once both are done the fresh
// chat's own recording is fetched and scored, and the comparison table
// (recorded vs new) is printed.
func TestRunEval_MultiTurnAndScored(t *testing.T) {
	var turnsSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"eval-c1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/eval-c1/responses":
			body, _ := io.ReadAll(r.Body)
			turnsSeen = append(turnsSeen, string(body))
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: node_start\ndata: {\"node_id\":\"n1\"}\n\n")
			io.WriteString(w, "event: node_done\ndata: {\"node_id\":\"n1\",\"output\":\"turn answer\"}\n\n")
			io.WriteString(w, "event: done\ndata: {}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/eval-c1/recording":
			w.Write(newRecordingBundle(t, "accuracy", 0.9))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	recorded := []replay.EvalScore{{Criterion: "accuracy", Score: 0.5}}
	var out, errOut bytes.Buffer
	code := RunEval(context.Background(), &out, &errOut, srv.URL, "coder", "new-model",
		[]string{"code-implementer"}, []string{"turn one", "turn two"}, recorded, "recorded final answer", false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
	if len(turnsSeen) != 2 {
		t.Fatalf("server saw %d turns, want 2 (sequential)", len(turnsSeen))
	}
	if !strings.Contains(turnsSeen[0], "turn one") || !strings.Contains(turnsSeen[1], "turn two") {
		t.Errorf("turns arrived out of order: %v", turnsSeen)
	}
	got := out.String()
	for _, want := range []string{"accuracy", "0.50", "0.90", "new-model"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestRunEval_TurnFails: a mid-conversation turn failure exits 1 and never
// reaches the recording fetch/comparison step.
func TestRunEval_TurnFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats":
			io.WriteString(w, `{"id":"eval-c2"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/eval-c2/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: error\ndata: {\"error\":\"boom\"}\n\n")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := RunEval(context.Background(), &out, &errOut, srv.URL, "all", "m", nil, []string{"only turn"}, nil, "", false)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// TestRunEval_RecordingUnavailable: the fresh chat's recording can't be
// fetched (disabled, or GC'd) - eval still completes and exits 0, with the
// new side reported as unscored rather than the whole command failing.
func TestRunEval_RecordingUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats":
			io.WriteString(w, `{"id":"eval-c3"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/eval-c3/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: node_done\ndata: {\"node_id\":\"n1\",\"output\":\"ok\"}\n\n")
			io.WriteString(w, "event: done\ndata: {}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/eval-c3/recording":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	recorded := []replay.EvalScore{{Criterion: "accuracy", Score: 0.5}}
	var out, errOut bytes.Buffer
	code := RunEval(context.Background(), &out, &errOut, srv.URL, "all", "m", nil, []string{"only turn"}, recorded, "recorded", false)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (missing recording is not an infra failure)", code)
	}
	if !strings.Contains(errOut.String(), "warning") {
		t.Errorf("stderr = %q, want a warning about the missing recording", errOut.String())
	}
	if !strings.Contains(out.String(), "n/a") {
		t.Errorf("stdout = %q, want the new-side score reported as n/a", out.String())
	}
}
