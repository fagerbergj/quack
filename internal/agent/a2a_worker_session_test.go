package agent

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
)

// countingSessions records every session Create call's (app, user, id) triple.
type countingSessions struct {
	session.Service
	created []string
}

func (c *countingSessions) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	resp, err := c.Service.Create(ctx, req)
	if err == nil && resp != nil && resp.Session != nil {
		c.created = append(c.created, req.AppName+"/"+req.UserID+"/"+resp.Session.ID())
	}
	return resp, err
}

// TestWorkerSessionsUseDeterministicIDs is a regression test for the ADK
// audit's A2 finding: every first dispatch to a node used to leave
// req.Message.ContextID empty, and a2a-go (a2asrv/agentexec.go
// createNewExecutionContext) then minted a fresh random UUID per node
// execution - an orphaned Postgres sessions/events row nothing could ever
// address again. scopeMessage now defaults ContextID to
// WorkerSessionID(chatID, nodeID), so each node's worker session lands under
// a stable, reap-able id instead.
func TestWorkerSessionsUseDeterministicIDs(t *testing.T) {
	workerSessions := &countingSessions{Service: session.InMemoryService()}
	parentSessions := session.InMemoryService()
	const chatID = "chat-1"

	for _, nodeID := range []string{"n1", "n2"} {
		srv, err := Serve(newWorker(t), workerSessions, nil, nil, Compaction{}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()

		contextID := WorkerSessionID(chatID, nodeID)
		client, err := srv.ClientForNode("plan1:"+nodeID, contextID)
		if err != nil {
			t.Fatal(err)
		}
		r, err := runner.New(runner.Config{
			AppName: "quack", Agent: client, SessionService: parentSessions, AutoCreateSession: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range r.Run(context.Background(), "u", "chat-1-"+nodeID,
			genai.NewContentFromText("go", genai.RoleUser), adkagent.RunConfig{}) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	if len(workerSessions.created) != 2 {
		t.Fatalf("worker sessions created = %d, want 2: %v", len(workerSessions.created), workerSessions.created)
	}
	wantN1 := "spike-worker/" + WorkerSessionUser(WorkerSessionID(chatID, "n1")) + "/" + WorkerSessionID(chatID, "n1")
	wantN2 := "spike-worker/" + WorkerSessionUser(WorkerSessionID(chatID, "n2")) + "/" + WorkerSessionID(chatID, "n2")
	got := map[string]bool{workerSessions.created[0]: true, workerSessions.created[1]: true}
	if !got[wantN1] || !got[wantN2] {
		t.Fatalf("worker sessions created = %v, want %q and %q", workerSessions.created, wantN1, wantN2)
	}
}
