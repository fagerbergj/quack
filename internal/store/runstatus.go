package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
)

// Chat.RunStatus values: a run's terminal outcome. Never "queued"/"running", which stay live in-memory
// signals a crashed process can't get stuck reporting.
const (
	RunStatusIdle       = "idle"
	RunStatusFailed     = "failed"
	RunStatusNeedsInput = "needs_input"
	// RunStatusPaused marks a chat whose nodes the server suspended (shutdown
	// drain, or a boot reconcile) and intends to resume itself - not failed.
	RunStatusPaused = "paused"
	// RunStatusInterruptedLegacy is the retired stamp; a pre-existing database
	// row can still carry it, so the read path keeps mapping it to failed.
	RunStatusInterruptedLegacy = "interrupted"
)

// MarkRunActive records turnID in flight so a crash before StampRunOutcome is detectable. Logs its own
// failure: callers discard the error, and a lost marker leaves nothing at boot to correct the status.
func (s *Store) MarkRunActive(ctx context.Context, chatID, turnID string) error {
	err := s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Update("active_turn_id", turnID).Error
	if err != nil {
		slog.Warn("mark run active failed; a crash during this run will not be reconciled at boot",
			"component", "store", "chat", chatID, "turn", turnID, "err", err)
	}
	return err
}

// StampRunOutcome records a run's terminal status on the chat row, clears the in-flight marker and bumps
// updated_at.
func (s *Store) StampRunOutcome(ctx context.Context, chatID, status, pendingQuestion string) error {
	return s.db.WithContext(ctx).Model(&Chat{}).Where("id = ?", chatID).Updates(map[string]any{
		"run_status":       status,
		"pending_question": pendingQuestion,
		"active_turn_id":   "",
		"updated_at":       time.Now().UTC(),
	}).Error
}

// DeriveTerminalStatus computes a chat's terminal status from its turns and pending question. nodeError is
// the failed node's DagNode.Error; chatID keys the failure trackers for give-ups that leave no DagNode.
func DeriveTerminalStatus(chatID string, turns []TurnContent, pendingQuestion string, hasPendingQuestion bool) (status, question, nodeError string) {
	if hasPendingQuestion {
		return RunStatusNeedsInput, pendingQuestion, ""
	}
	if n := len(turns); n > 0 {
		last := turns[n-1]
		if strings.TrimSpace(last.AsstText) == "" {
			if errText, failed := failedDagNodeError(last.Nodes); failed {
				return RunStatusFailed, "", errText
			}
			// A DB failure that survived pgdial's retries is more specific evidence than a gateway or rejection reason.
			if reason, failed := inference.LastStoreFailure(chatID); failed {
				return RunStatusFailed, "", fmt.Sprintf("database unavailable: %s", reason)
			}
			// A gateway outage during this turn is stronger evidence than an earlier rejection on the same turn.
			if errText, failed := orchestratorGiveUpError(chatID); failed {
				return RunStatusFailed, "", errText
			}
			if reason, failed := inference.LastPlanRejection(chatID); failed {
				return RunStatusFailed, "", reason
			}
		}
	}
	return RunStatusIdle, "", ""
}

// ChatsWithRunningNode reports which of chatIDs have a running node in their latest plan -
// the signal a resumed node needs before the in-memory Hub catches up; one query per list page.
func (s *Store) ChatsWithRunningNode(ctx context.Context, chatIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(chatIDs) == 0 {
		return out, nil
	}
	var ids []string
	err := s.db.WithContext(ctx).Table("dag_nodes").
		Joins("JOIN dag_plans ON dag_plans.id = dag_nodes.plan_id").
		Where("dag_nodes.status = ? AND dag_plans.chat_id IN ?", string(dag.StatusRunning), chatIDs).
		Where("dag_plans.created_at = (SELECT MAX(p2.created_at) FROM dag_plans p2 WHERE p2.chat_id = dag_plans.chat_id)").
		Distinct().Pluck("dag_plans.chat_id", &ids).Error
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// orchestratorGiveUpError reports the gateway error when the orchestrator's planning loop failed every call,
// keyed by an empty node/agent. Doesn't clear the tracker: it runs on every read, and the next call supersedes it.
func orchestratorGiveUpError(chatID string) (string, bool) {
	err, streak, dur, ok := inference.LastFailure(chatID, "", "")
	if !ok || streak == 0 {
		return "", false
	}
	class, _ := inference.SanitizeGatewayError(err)
	return fmt.Sprintf("%s on %d consecutive attempts over %s", class, streak, dur.Round(time.Second)), true
}

// StampTerminalOutcome derives the newest turn's terminal status and persists it: every dispatch path's tail
// once events stop draining. Loads only the last turn, all DeriveTerminalStatus reads.
func (s *Store) StampTerminalOutcome(ctx context.Context, appName, userID, chatID string, pendingQuestion func() (string, bool)) (status, question, nodeError string) {
	last, err := s.GetLastTurnWithContent(ctx, appName, userID, chatID)
	if err != nil {
		slog.Warn("stamp terminal outcome: turn load failed", "component", "store", "chat", chatID, "err", err)
	}
	var turns []TurnContent
	if last != nil {
		turns = []TurnContent{*last}
	}
	q, hasQ := pendingQuestion()
	status, question, nodeError = DeriveTerminalStatus(chatID, turns, q, hasQ)
	if err := s.StampRunOutcome(ctx, chatID, status, question); err != nil {
		slog.Warn("stamp terminal outcome: persist failed", "component", "store", "chat", chatID, "err", err)
	}
	return status, question, nodeError
}

// ScanOrphanedRuns reconciles chats a killed process left mid-run: paused if
// a node is suspended, else stamped failed once (settleInterrupted).
func (s *Store) ScanOrphanedRuns(ctx context.Context) (paused, noResumableNode []string, err error) {
	var chats []Chat
	if err := s.db.WithContext(ctx).
		Where("active_turn_id <> ? OR run_status = ?", "", RunStatusPaused).
		Find(&chats).Error; err != nil {
		return nil, nil, err
	}
	suspended, err := s.chatsWithPausedNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range chats {
		if suspended[c.ID] {
			paused = append(paused, c.ID)
			if err := s.stampRunStatusKeepingQuestion(ctx, c.ID, RunStatusPaused); err != nil {
				slog.Warn("scan orphaned runs: stamp failed", "component", "store", "chat", c.ID, "err", err)
			}
			continue
		}
		if c.ActiveTurnID != "" {
			noResumableNode = append(noResumableNode, c.ID)
			s.settleInterrupted(ctx, c.ID)
		}
	}
	return paused, noResumableNode, nil
}

// settleInterrupted stamps a crashed run with nothing to resume failed (clearing ActiveTurnID,
// so the next boot doesn't report it again) and fails its still-open dag_node records.
func (s *Store) settleInterrupted(ctx context.Context, chatID string) {
	if err := s.stampRunStatusKeepingQuestion(ctx, chatID, RunStatusFailed); err != nil {
		slog.Warn("scan orphaned runs: interrupted stamp failed", "component", "store", "chat", chatID, "err", err)
	}
	if err := dag.FailOpenDagNodeRecords(ctx, s.artifacts, chatAppName, s.SessionUserForChat(ctx, chatID), chatID); err != nil {
		slog.Warn("scan orphaned runs: dag_node records not settled", "component", "store", "chat", chatID, "err", err)
	}
}

// stampRunStatusKeepingQuestion is StampRunOutcome minus the pending_question
// write - see ScanOrphanedRuns.
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

// failedDagNodeError reports the first failed node's error so a recorded gateway failure explains "failed".
// dag.SilentGapError reports "": that sentinel is for the row, not public text.
func failedDagNodeError(nodes []DagNode) (errText string, failed bool) {
	for _, n := range nodes {
		if n.Status == "failed" {
			if n.Error == dag.SilentGapError {
				return "", true
			}
			return n.Error, true
		}
	}
	return "", false
}

// SeedOutputs is a re-run's seed from a plan's node rows: every finished output, plus which
// of them never passed review (stopped by the user, or rejected by the judge).
func SeedOutputs(nodes []DagNode) (seeded map[string]string, unreviewed map[string]bool) {
	seeded, unreviewed = make(map[string]string, len(nodes)), map[string]bool{}
	for _, n := range nodes {
		if n.Output == "" {
			continue
		}
		seeded[n.NodeID] = n.Output
		if n.Status == string(dag.StatusCancelled) || n.JudgeRounds > 0 && !n.JudgePassed {
			unreviewed[n.NodeID] = true
		}
	}
	return seeded, unreviewed
}
