package serve

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// planningCutChat is a chat a restart cut while it planned: in flight, a started transcript, no plan.
func planningCutChat(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx, chatID := context.Background(), "chat-cut-planning"
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
		t.Fatal(err)
	}
	for i, ev := range []stream.SSEEvent{stream.ResponseCreated("turn-1"), stream.NodeQueued("n1")} {
		js, _ := runlog.MarshalEvent(ev)
		if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: int64(i + 1), Event: js}); err != nil {
			t.Fatal(err)
		}
	}
	return st, chatID
}

// A chat boot settles failed ends its transcript with the failure and a done.
func TestEndSettledStreamsEndsTheTranscriptFailed(t *testing.T) {
	ctx := context.Background()
	st, chatID := planningCutChat(t)
	resume := reconcileNodes(ctx, st, nil, nil)
	if len(resume.Settled) != 1 || resume.Settled[0].ChatID != chatID {
		t.Fatalf("Settled = %+v, want the cut chat", resume.Settled)
	}
	hub := stream.NewHub()

	endSettledStreams(ctx, resume.Settled, hub, runlog.NewEventLog(st))

	if _, _, cancel, done := hub.Subscribe(chatID); !done {
		cancel()
		t.Error("the settled chat's hub topic is still open")
	}
	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range evs {
		ev, _ := runlog.UnmarshalEvent(e.Event)
		names = append(names, ev.Name)
	}
	if got := strings.Join(names, ","); got != "response_created,node_queued,error,done" {
		t.Errorf("transcript = %s, want response_created,node_queued,error,done", got)
	}
}

// A `chat show -f` following a run when boot settles it ends on the failure (exit 1), not "lost connection".
func TestFollowerOfASettledChatExitsFailed(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	ctx := context.Background()
	st, chatID := planningCutChat(t)
	hub := stream.NewHub()
	h := rest.NewHandler(st, nil, nil, nil, hub, nil, "test", nil, nil, nil, nil)
	var subscribes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chats/"+chatID, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"`+chatID+`","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","system_prompt":"","status":"running","turns":[]}`)
	})
	mux.HandleFunc("/api/v1/chats/"+chatID+"/stream", func(w http.ResponseWriter, r *http.Request) {
		subscribes.Add(1)
		h.SubscribeChatStream(w, r, chatID)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing() // before srv.Close, which waits out a follower still attached

	var out, errOut bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- cli.RunChatShow(followCtx, &out, &errOut, srv.URL, chatID, false, true) }()
	for subscribes.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// The follower's first attempt replayed a transcript with no end; boot settles the chat before it reconnects.
	endSettledStreams(ctx, reconcileNodes(ctx, st, nil, nil).Settled, hub, runlog.NewEventLog(st))

	select {
	case c := <-code:
		if c != 1 || !strings.Contains(errOut.String(), settledStreamError) || strings.Contains(errOut.String(), "lost connection") {
			t.Errorf("exit %d, stderr %q; want 1 with the settled failure", c, errOut.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the follower never ended")
	}
}
