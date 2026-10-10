package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/fagerbergj/quack/internal/dag"
)

// ErrIllegalTransition is returned by SetNodeStatus for a move the node
// lifecycle doesn't allow (e.g. done → running).
var ErrIllegalTransition = errors.New("store: illegal node-status transition")

func (s *Store) planForChat(ctx context.Context, chatID string) (string, error) {
	p, err := s.GetLatestDagPlan(ctx, chatID)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("store: no dag plan for chat %s", chatID)
	}
	return p.ID, nil
}

// SetNodeStatus validates from -> to against dag's table and writes status + pause metadata synchronously, so
// a pause is on disk before it's acted on. Empty to stamps only the pause metadata (the HITL park).
func (s *Store) SetNodeStatus(ctx context.Context, planID, nodeID string, to dag.NodeStatus, reason dag.PauseReason, question string) error {
	fields := map[string]any{"pause_reason": string(reason), "pending_question": question}
	if to == "" {
		return s.db.WithContext(ctx).Model(&DagNode{}).
			Where("plan_id = ? AND node_id = ?", planID, nodeID).Updates(fields).Error
	}
	prev, err := s.GetDagNode(ctx, planID, nodeID)
	if err != nil {
		return err
	}
	from := dag.StatusQueued
	if prev != nil {
		from = dag.NodeStatus(prev.Status)
	}
	if from != to && !dag.CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s (node %s)", ErrIllegalTransition, from, to, nodeID)
	}
	return s.casNodeStatus(ctx, planID, nodeID, from, to, fields)
}

// casNodeStatus commits only if status still equals from, so a stale legality check (a pause racing the
// node's own done write) never overwrites the newer status.
func (s *Store) casNodeStatus(ctx context.Context, planID, nodeID string, from, to dag.NodeStatus, fields map[string]any) error {
	fields["status"] = string(to)
	res := s.db.WithContext(ctx).Model(&DagNode{}).
		Where("plan_id = ? AND node_id = ? AND status = ?", planID, nodeID, string(from)).
		Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s → %s (node %s) lost the race: status changed underneath it", ErrIllegalTransition, from, to, nodeID)
	}
	return nil
}

// SetNodeStatusForChat is SetNodeStatus keyed the way the control plane
// speaks (chat + node), resolving the chat's latest plan.
func (s *Store) SetNodeStatusForChat(ctx context.Context, chatID, nodeID, status, pauseReason, pendingQuestion string) error {
	planID, err := s.planForChat(ctx, chatID)
	if err != nil {
		return err
	}
	return s.SetNodeStatus(ctx, planID, nodeID, dag.NodeStatus(status), dag.PauseReason(pauseReason), pendingQuestion)
}

// SetNodeQueue persists a node's steer queue as JSON.
func (s *Store) SetNodeQueue(ctx context.Context, chatID, nodeID, queueJSON string) error {
	planID, err := s.planForChat(ctx, chatID)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&DagNode{}).
		Where("plan_id = ? AND node_id = ?", planID, nodeID).
		Update("queued_messages", queueJSON).Error
}

// GetNodeState reads back what a resume needs. Missing row → all empty, nil error.
func (s *Store) GetNodeState(ctx context.Context, chatID, nodeID string) (status, pauseReason, pendingQuestion, queueJSON string, err error) {
	planID, err := s.planForChat(ctx, chatID)
	if err != nil {
		return "", "", "", "", err
	}
	n, err := s.GetDagNode(ctx, planID, nodeID)
	if err != nil || n == nil {
		return "", "", "", "", err
	}
	return n.Status, n.PauseReason, n.PendingQuestion, n.QueuedMessages, nil
}

// ListPausedDagNodes returns every suspended node (paused or legacy needs_input) across all plans.
func (s *Store) ListPausedDagNodes(ctx context.Context) ([]DagNode, error) {
	var out []DagNode
	err := s.db.WithContext(ctx).
		Where("status IN ?", []string{string(dag.StatusPaused), string(dag.StatusNeedsInput)}).
		Find(&out).Error
	return out, err
}
