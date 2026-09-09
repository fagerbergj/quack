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

// TestPerNodeServersTrackReapsWorkerSession is a regression test for the ADK
// audit's A2 finding: a node's A2A worker session (internal/agent.
// WorkerSessionID) used to be created and never deleted, leaking a Postgres
// sessions/events row for every DAG node execution ever run. track()'s
// release must now delete that exact (app, user, id) triple alongside
// closing the A2A server.
func TestPerNodeServersTrackReapsWorkerSession(t *testing.T) {
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
	srv, err := agent.Serve(worker, session.InMemoryService(), nil, nil, agent.Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	p := newPerNodeServers()
	release := p.track(srv, sessions, appName, userID, sessID)
	release(false)

	if _, err := sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessID}); err == nil {
		t.Fatal("worker session still present after release(false)")
	}

	// Idempotent: a second release() (e.g. shutdown's closeAll racing a
	// node's own release) must not panic or error on the already-deleted row.
	release(false)
}

// TestPerNodeServersTrackKeepsSessionOnPause is a regression test for the
// HITL-park case: release(true) must close the A2A server (a resume gets a
// fresh one anyway) but leave the deterministic worker session alone, so a
// resumed dispatch to the SAME session id still finds its prior history.
func TestPerNodeServersTrackKeepsSessionOnPause(t *testing.T) {
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
	srv, err := agent.Serve(worker, session.InMemoryService(), nil, nil, agent.Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	p := newPerNodeServers()
	release := p.track(srv, sessions, appName, userID, sessID)
	release(true)

	if _, err := sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessID}); err != nil {
		t.Fatalf("worker session reaped despite release(true): %v", err)
	}
}
