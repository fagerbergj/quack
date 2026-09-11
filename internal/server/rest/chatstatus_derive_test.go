package rest

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// TestChatStatus_RunningNodeReadsRunningWithoutHub pins that chat status
// derives from the latest plan's node rows, not only the in-memory Hub.
func TestChatStatus_RunningNodeReadsRunningWithoutHub(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	chatID, planID, nodeID := "chat-1", "plan-1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	if h.hub.Active(chatID) {
		t.Fatal("test setup: hub must have no live signal for this chat")
	}
	status, _ := h.chatStatus(ctx, chatID, nil)
	if status != schema.ChatStatusRunning {
		t.Errorf("status = %q, want running (derived from the node row, not the Hub)", status)
	}
}

// TestChatStatus_NoNodesReadsIdle is TestChatStatus_RunningNodeReadsRunningWithoutHub's
// negative case: a chat with a plan but no running node falls through to idle.
func TestChatStatus_NoNodesReadsIdle(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	chatID, planID, nodeID := "chat-2", "plan-2", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: nodeID, PlanID: planID, Status: "done"}); err != nil {
		t.Fatalf("seed done node: %v", err)
	}

	status, _ := h.chatStatus(ctx, chatID, nil)
	if status != schema.ChatStatusIdle {
		t.Errorf("status = %q, want idle", status)
	}
}
