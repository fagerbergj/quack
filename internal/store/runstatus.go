package store

import (
	"context"
	"log/slog"
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
	// RunStatusInterrupted marks a run the server cut short itself (shutdown's
	// force-cancel, or a boot scan finding a killed process's leftover) -
	// distinct from RunStatusFailed; wire-surfaces as ChatStatusFailed.
	RunStatusInterrupted = "interrupted"
	// RunStatusPaused marks a chat whose nodes the server itself suspended
	// (shutdown drain, or a hard kill reconciled at boot) and intends to
	// resume on its own. Distinct from RunStatusInterrupted: nothing is asked
	// of the user, so it must not wire-surface as failed (#962).
	RunStatusPaused = "paused"
)

// MarkRunActive records that chatID has turnID in flight, so a crash before StampRunOutcome
// runs is detectable: ActiveTurnID left non-empty with no live hub/queue signal (#738).
//
// Logs its own failure: every caller discards the error, and a lost marker is
// invisible to ScanOrphanedRuns - the chat then keeps whatever status the run
// BEFORE it stamped, with nothing at boot to correct it (#920).
func (s *Store) MarkRunActive(ctx context.Context, chatID, turnID string) error {
	err := s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Update("active_turn_id", turnID).Error
	if err != nil {
		slog.Warn("mark run active failed; a crash during this run will not be reconciled at boot",
			"component", "store", "chat", chatID, "turn", turnID, "err", err)
	}
	return err
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

// StampTerminalOutcome loads turns, derives the terminal status via
// DeriveTerminalStatus, and persists it - the shared tail every dispatch
// path (REST, SDK extensions, GitHub webhook) runs once a run's events stop
// draining. pendingQuestion is the caller's own PendingQuestion lookup, kept
// as a func value so callers don't need to share a concrete runner type.
func (s *Store) StampTerminalOutcome(ctx context.Context, appName, userID, chatID string, pendingQuestion func() (string, bool)) (status, question string) {
	turns, err := s.GetTurnsWithContent(ctx, appName, userID, chatID)
	if err != nil {
		slog.Warn("stamp terminal outcome: turns load failed", "component", "store", "chat", chatID, "err", err)
	}
	q, hasQ := pendingQuestion()
	status, question = DeriveTerminalStatus(turns, q, hasQ)
	if err := s.StampRunOutcome(ctx, chatID, status, question); err != nil {
		slog.Warn("stamp terminal outcome: persist failed", "component", "store", "chat", chatID, "err", err)
	}
	return status, question
}

// ScanOrphanedRuns reconciles every chat a killed process left mid-run
// (ActiveTurnID set, or already interrupted/paused). A chat with suspended
// nodes is stamped RunStatusPaused - the server resumes those itself, so
// calling it interrupted would tell the user to resend a message that is
// already coming back. Everything else keeps the old RunStatusInterrupted.
//
// It deliberately does not touch pending_question: the node row owns the HITL
// question now, and blanking the chat's copy destroyed resume state (#957).
// Returns the paused chat ids and the interrupted ones, for the caller to log.
//
// Startup-only: the scan is table-wide with no per-chat liveness check, so
// calling it once the Hub has registered runs would stamp a live chat.
func (s *Store) ScanOrphanedRuns(ctx context.Context) (paused, interrupted []string, err error) {
	var chats []Chat
	if err := s.db.WithContext(ctx).
		Where("active_turn_id <> ? OR run_status IN ?", "", []string{RunStatusInterrupted, RunStatusPaused}).
		Find(&chats).Error; err != nil {
		return nil, nil, err
	}
	suspended, err := s.chatsWithPausedNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range chats {
		status := RunStatusInterrupted
		if suspended[c.ID] {
			status = RunStatusPaused
			paused = append(paused, c.ID)
		} else {
			interrupted = append(interrupted, c.ID)
		}
		if err := s.stampRunStatusKeepingQuestion(ctx, c.ID, status); err != nil {
			slog.Warn("scan orphaned runs: stamp failed", "component", "store", "chat", c.ID, "err", err)
		}
	}
	return paused, interrupted, nil
}

// stampRunStatusKeepingQuestion is StampRunOutcome minus the pending_question
// write - see ScanOrphanedRuns (#957).
func (s *Store) stampRunStatusKeepingQuestion(ctx context.Context, chatID, status string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Updates(map[string]any{
		"run_status":     status,
		"active_turn_id": "",
		"updated_at":     time.Now().UTC(),
	}).Error
}

// chatsWithPausedNodes indexes the chats that currently own a suspended node.
func (s *Store) chatsWithPausedNodes(ctx context.Context) (map[string]bool, error) {
	nodes, err := s.ListPausedDagNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, n := range nodes {
		var p DagPlan
		if e := s.db.WithContext(ctx).Where("id = ?", n.PlanID).First(&p).Error; e == nil {
			out[p.ChatID] = true
		}
	}
	return out, nil
}

func hasFailedDagNode(nodes []DagNode) bool {
	for _, n := range nodes {
		if n.Status == "failed" {
			return true
		}
	}
	return false
}
