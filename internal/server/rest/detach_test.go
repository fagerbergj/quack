package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/stream"
)

// A run is SERVER-SIDE work; an SSE client is just a viewer. These tests pin
// that: whatever the client does - stops reading (a sleeping laptop), drops the
// connection (a closed tab, a killed curl) - the run keeps executing to
// completion. Only the explicit cancel endpoint may kill it.

// gatedModel is a model.LLM whose reply is gated on a channel: it announces the
// call on started, then waits for unblock (or ctx cancellation, which it
// reports back as a cancelled call). reply is padded to size bytes so a run can
// out-write a client that never reads.
type gatedModel struct {
	started   chan struct{}
	unblock   chan struct{}
	cancelled chan struct{}
	size      int
}

func newGatedModel(size int) *gatedModel {
	return &gatedModel{
		started:   make(chan struct{}, 1),
		unblock:   make(chan struct{}),
		cancelled: make(chan struct{}, 1),
		size:      size,
	}
}

func (m *gatedModel) Name() string { return "gated" }

func (m *gatedModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		select {
		case m.started <- struct{}{}:
		default:
		}
		select {
		case <-m.unblock:
		case <-ctx.Done():
			select {
			case m.cancelled <- struct{}{}:
			default:
			}
			yield(nil, ctx.Err())
			return
		}
		text := "done-" + strings.Repeat("x", m.size)
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// runServer exposes SendChatMessage for chatID over a real TCP listener, so a
// test can be a real (or deliberately misbehaving) HTTP client.
func runServer(t *testing.T, h *Handler, chatID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.SendChatMessage(w, r, chatID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// watch attaches a second viewer to the chat's live run through the hub - the
// same path SubscribeChatStream serves - and reports whether the run reaches its
// terminal `done` event within d.
func watch(t *testing.T, h *Handler, chatID string, d time.Duration) bool {
	t.Helper()
	deadline := time.After(d)
	for { // wait for the run to open its topic, then tail it
		if h.hub.Active(chatID) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("run never started (hub topic never opened)")
		case <-time.After(5 * time.Millisecond):
		}
	}
	replay, live, cancel, done := h.hub.Subscribe(chatID)
	defer cancel()
	for _, it := range replay {
		if it.SSE.Name == stream.EventDone {
			return true
		}
	}
	if done {
		return false
	}
	for {
		select {
		case it, ok := <-live:
			if !ok {
				return false
			}
			if it.SSE.Name == stream.EventDone {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestRunSurvivesClientThatStopsReading: the sleeping-laptop case. The client
// opens the stream and never reads a byte; the run's reply is far larger than
// the socket buffers, so writing to that client blocks forever. The run must
// still finish - it must not be pulled by (and stall behind) the SSE write.
func TestRunSurvivesClientThatStopsReading(t *testing.T) {
	m := newGatedModel(4 << 20) // 4MB reply: dwarfs any kernel/socket buffer
	h := newTestHandlerWithModel(t, m)
	chatID := mustCreateChat(t, h)
	srv := runServer(t, h, chatID)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	body := `{"content":"hello"}`
	if _, err := fmt.Fprintf(conn, "POST /api/v1/chats/c1/responses HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// This client never reads the response. Let the run get going, then let the
	// model answer with its oversized reply.
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never called")
	}
	close(m.unblock)

	if !watch(t, h, chatID, 20*time.Second) {
		t.Fatal("run did not complete: it stalled behind an SSE client that stopped reading")
	}
}

// TestRunSurvivesClientDisconnect: the dropped-curl / closed-tab case. The
// client disconnects mid-run; the run must continue to completion and stay
// watchable by anyone who attaches.
func TestRunSurvivesClientDisconnect(t *testing.T) {
	m := newGatedModel(0)
	h := newTestHandlerWithModel(t, m)
	chatID := mustCreateChat(t, h)
	srv := runServer(t, h, chatID)

	ctx, abort := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/chats/c1/responses", strings.NewReader(`{"content":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never called")
	}
	// The viewer goes away mid-run (aborting the request closes the connection).
	abort()
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	close(m.unblock) // the model answers only AFTER the client is gone

	if !watch(t, h, chatID, 20*time.Second) {
		t.Fatal("run did not complete after its SSE client disconnected")
	}
	select {
	case <-m.cancelled:
		t.Fatal("the run's context was cancelled by the client disconnect")
	default:
	}
}

// TestExplicitCancelStillKillsRun: decoupling the run from the request must not
// cost us the one legitimate way to stop it - PUT .../responses/{id}/status
// {"status":"cancelled"}.
func TestExplicitCancelStillKillsRun(t *testing.T) {
	m := newGatedModel(0) // never unblocked: only a cancel can end this run
	h := newTestHandlerWithModel(t, m)
	chatID := mustCreateChat(t, h)
	srv := runServer(t, h, chatID)

	resp, err := http.Post(srv.URL+"/api/v1/chats/c1/responses", "application/json", strings.NewReader(`{"content":"hello"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	responseID := readResponseID(t, resp)

	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never called")
	}

	b, _ := json.Marshal(schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/chats/"+chatID+"/responses/"+responseID+"/status", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.UpdateResponseStatus(rec, req, chatID, responseID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-m.cancelled:
	case <-time.After(20 * time.Second):
		t.Fatal("explicit cancel did not cancel the run")
	}
}

// readResponseID reads the stream's opening response_created event and returns
// the run's response id (the id the cancel endpoint names).
func readResponseID(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	var acc string
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		acc += string(buf[:n])
		if i := strings.Index(acc, "\n\n"); i >= 0 {
			for _, line := range strings.Split(acc[:i], "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var d struct {
					ResponseID string `json:"response_id"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err == nil && d.ResponseID != "" {
					return d.ResponseID
				}
			}
			t.Fatalf("first SSE event carried no response_id: %q", acc[:i])
		}
		if err != nil {
			break
		}
	}
	t.Fatal("no response_created event")
	return ""
}
