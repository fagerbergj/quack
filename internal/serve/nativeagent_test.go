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

// TestPerNodeServersTrackNeverReapsWorkerSession pins the continue
// primitive's requirement: a node's A2A worker session (internal/agent.
// WorkerSessionID) must survive release() regardless of paused - a LATER
// node's continue: may point its own session at this exact one (see
// dag.buildGateNodes' sessionNodeID), so track() no longer deletes it here
// at all; store.ReapNodeSessions (wired to chat archive/delete) is the real
// teardown now. Only the A2A listener itself is torn down.
func TestPerNodeServersTrackNeverReapsWorkerSession(t *testing.T) {
	ctx := context.Background()
	sessions := session.InMemoryService()
	const appName, userID, sessID = "worker-bundle", "A2A_USER_chat-1:n1", "chat-1:n1"
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessID}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	for _, paused := range []bool{false, true} {
		worker, err := llmagent.New(llmagent.Config{Name: "w", Description: "w", Model: stubLLM{}})
		if err != nil {
			t.Fatal(err)
		}
		srv, err := agent.Serve(worker, sessions, nil, nil, agent.Compaction{}, "", nil)
		if err != nil {
			t.Fatal(err)
		}

		p := newPerNodeServers()
		release := p.track(srv)
		release(paused)
		// Idempotent: a second release() (e.g. shutdown's closeAll racing a
		// node's own release) must not panic.
		release(paused)

		if _, err := sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessID}); err != nil {
			t.Fatalf("paused=%v: worker session reaped by release() - it must only be, via store.ReapNodeSessions: %v", paused, err)
		}
	}
}
