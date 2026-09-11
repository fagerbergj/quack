package serve

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/agent"
)

// stubLLM never needs to answer - track()'s release never invokes the model.
type stubLLM struct{}

func (stubLLM) Name() string { return "stub" }
func (stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

// TestPerNodeServersTrackNeverReapsWorkerSession: node reuse needs a node's
// A2A worker session (internal/agent.WorkerSessionID) to survive past a
// single dispatch, paused or not - only chat archive/delete
// (store.ReapNodeSessions) reaps it now. release must still close the A2A
// server, and stay idempotent, without touching the session at all.
func TestPerNodeServersTrackNeverReapsWorkerSession(t *testing.T) {
	ctx := context.Background()
	sessions := session.InMemoryService()
	const appName, userID, sessID = "worker-bundle", "A2A_USER_chat-1:n1", "chat-1:n1"
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessID}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	worker, err := llmagent.New(llmagent.Config{Name: "w", Description: "w", Model: stubLLM{}})
	if err != nil {
		t.Fatal(err)
	}

	for _, paused := range []bool{false, true} {
		srv, err := agent.Serve(worker, session.InMemoryService(), nil, nil, agent.Compaction{}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		p := newPerNodeServers()
		release := p.track(srv)
		release(paused)

		if _, err := sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessID}); err != nil {
			t.Fatalf("release(%v) reaped the worker session: %v", paused, err)
		}
		// Idempotent: a second release() (e.g. shutdown's closeAll racing a
		// node's own release) must not panic.
		release(paused)
	}
}
