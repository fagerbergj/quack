package dag

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

func seedDagNode(t *testing.T, svc artifact.Service, rec DagNodeRecord) *recordstore.Client {
	t.Helper()
	c := recordstore.New(svc, "quack", "u1", "chat1")
	if _, _, err := c.SaveStructured(context.Background(), kindDagNode, rec, rec.NodeID, recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}
	return c
}

// TestUpdateDagNodeStatusRejectsIllegalTransition: a stale read racing an
// interleaved cancel/done pair must not leave the record on a non-terminal
// status forever - CanTransition gates the write, not just logs a warning
// (mirrors runlog.PersistNodeEvent's own store-row check).
func TestUpdateDagNodeStatusRejectsIllegalTransition(t *testing.T) {
	SetAgentRoster([]AgentInfo{{Name: "code-implementer"}})
	svc := artifact.InMemoryService()
	seedDagNode(t, svc, DagNodeRecord{NodeID: "n1", Agent: "code-implementer", Status: StatusQueued})

	if err := UpdateDagNodeStatus(context.Background(), svc, "quack", "u1", "chat1", "n1", StatusDone); err == nil {
		t.Fatal("want an error for the illegal queued -> done jump, got nil")
	}

	c := recordstore.New(svc, "quack", "u1", "chat1")
	raw, _, ok, err := c.Latest(context.Background(), kindDagNode+":n1")
	if err != nil || !ok {
		t.Fatalf("read back dag_node: ok=%v err=%v", ok, err)
	}
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusQueued {
		t.Errorf("status = %q, want it to stay queued after a rejected transition", rec.Status)
	}
}

// TestUpdateDagNodeContextRoundTrips: the ACP transport session id learned at
// node completion must be readable back off the record - what execute reads
// to seed a later reuse's session/load.
func TestUpdateDagNodeContextRoundTrips(t *testing.T) {
	SetAgentRoster([]AgentInfo{{Name: "code-implementer"}})
	svc := artifact.InMemoryService()
	seedDagNode(t, svc, DagNodeRecord{NodeID: "n1", Agent: "code-implementer", ContextID: "chat1:n1"})

	if err := UpdateDagNodeContext(context.Background(), svc, "quack", "u1", "chat1", "n1", "acp-sess-abc"); err != nil {
		t.Fatalf("UpdateDagNodeContext: %v", err)
	}

	c := recordstore.New(svc, "quack", "u1", "chat1")
	raw, _, ok, err := c.Latest(context.Background(), kindDagNode+":n1")
	if err != nil || !ok {
		t.Fatalf("read back dag_node: ok=%v err=%v", ok, err)
	}
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.ContextID != "acp-sess-abc" {
		t.Errorf("ContextID = %q, want the newly learned transport session id", rec.ContextID)
	}
}

// TestUpdateDagNodeContextEmptyIsNoop: a native node (whose ACPSessionID
// never gets learned) must not clobber its own stable A2A context id.
func TestUpdateDagNodeContextEmptyIsNoop(t *testing.T) {
	SetAgentRoster([]AgentInfo{{Name: "code-implementer"}})
	svc := artifact.InMemoryService()
	seedDagNode(t, svc, DagNodeRecord{NodeID: "n1", Agent: "code-implementer", ContextID: "chat1:n1"})

	if err := UpdateDagNodeContext(context.Background(), svc, "quack", "u1", "chat1", "n1", ""); err != nil {
		t.Fatalf("UpdateDagNodeContext: %v", err)
	}

	c := recordstore.New(svc, "quack", "u1", "chat1")
	raw, _, ok, err := c.Latest(context.Background(), kindDagNode+":n1")
	if err != nil || !ok {
		t.Fatalf("read back dag_node: ok=%v err=%v", ok, err)
	}
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.ContextID != "chat1:n1" {
		t.Errorf("ContextID = %q, want the original native context id untouched", rec.ContextID)
	}
}

// TestDagNodeRecordResumable covers list_nodes' resumable/reason mapping off
// a node's stored status, for a node that actually started running at least once.
func TestDagNodeRecordResumable(t *testing.T) {
	cases := []struct {
		status    NodeStatus
		resumable bool
	}{
		{StatusQueued, false},
		{StatusRunning, false},
		{StatusPaused, false},
		{StatusNeedsInput, false},
		{StatusDone, true},
		{StatusFailed, true},
		{StatusCancelled, true},
	}
	for _, c := range cases {
		rec := DagNodeRecord{NodeID: "n1", Agent: "code-implementer", Status: c.status, Started: true}
		resumable, reason := rec.Resumable()
		if resumable != c.resumable {
			t.Errorf("status %q: resumable = %v, want %v", c.status, resumable, c.resumable)
		}
		if reason == "" {
			t.Errorf("status %q: reason is empty, want an explanation either way", c.status)
		}
	}
}

// TestDagNodeRecordResumable_NeverStarted: CanTransition allows queued ->
// failed/cancelled directly (an admission/setup failure, or a cancel before
// dispatch), so a terminal status alone does NOT prove a real session ever
// existed - Started must gate it, for both transports, or an ACP node's
// leftover mint-time placeholder gets threaded into session/load as a
// doomed "resume".
func TestDagNodeRecordResumable_NeverStarted(t *testing.T) {
	for _, status := range []NodeStatus{StatusDone, StatusFailed, StatusCancelled} {
		rec := DagNodeRecord{NodeID: "n1", Agent: "code-implementer", Status: status, Started: false}
		resumable, reason := rec.Resumable()
		if resumable {
			t.Errorf("status %q, never started: resumable = true, want false", status)
		}
		if reason != "no session recorded" {
			t.Errorf("status %q, never started: reason = %q, want %q", status, reason, "no session recorded")
		}
	}
}
