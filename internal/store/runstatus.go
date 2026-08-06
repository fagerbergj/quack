package store

import (
	"context"
	"strings"
	"time"
)

// Chat.RunStatus values - a run's terminal outcome. Never "queued"/"running": those stay
// live, in-memory-only signals (hub/orchestrator), not something a crashed process can get
// stuck reporting (#738).
const (
	RunStatusIdle       = "idle"
	RunStatusFailed     = "failed"
	RunStatusNeedsInput = "needs_input"
)

// MarkRunActive records that chatID has turnID in flight, so a crash before StampRunOutcome
// runs is detectable: ActiveTurnID left non-empty with no live hub/queue signal (#738).
func (s *Store) MarkRunActive(ctx context.Context, chatID, turnID string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Update("active_turn_id", turnID).Error
}

// StampRunOutcome records a run's terminal status on the chat row and clears the in-flight
// marker, replacing Touch (also bumps updated_at) - the read path uses this instead of
// recomputing status per chat (#738).
func (s *Store) StampRunOutcome(ctx context.Context, chatID, status, pendingQuestion string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Updates(map[string]any{
		"run_status":       status,
		"pending_question": pendingQuestion,
		"active_turn_id":   "",
		"updated_at":       time.Now().UTC(),
	}).Error
}

// DeriveTerminalStatus computes a chat's terminal status from its turns and whether a
// question is still pending. Shared by the REST and GitHub run drivers so both stamp the
// same rule the read path relies on (#738).
func DeriveTerminalStatus(turns []TurnContent, pendingQuestion string, hasPendingQuestion bool) (status, question string) {
	if hasPendingQuestion {
		return RunStatusNeedsInput, pendingQuestion
	}
	if n := len(turns); n > 0 {
		last := turns[n-1]
		if strings.TrimSpace(last.AsstText) == "" && hasFailedDagNode(last.Nodes) {
			return RunStatusFailed, ""
		}
	}
	return RunStatusIdle, ""
}

func hasFailedDagNode(nodes []DagNode) bool {
	for _, n := range nodes {
		if n.Status == "failed" {
			return true
		}
	}
	return false
}
