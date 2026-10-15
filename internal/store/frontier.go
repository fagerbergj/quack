package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
)

// addQueuedFrontier re-arms, in each chat a killed process left mid-run, the latest plan's not-started nodes
// whose dependencies are all done. No paused ancestor's retry reaches them, so without this the run fails.
func (s *Store) addQueuedFrontier(ctx context.Context, resumable func(chatID, pauseReason string) (bool, string), rep *ResumeReport) {
	var chats []Chat
	if err := s.db.WithContext(ctx).Where("active_turn_id <> ? AND id NOT LIKE ?", "", ExperimentChatPrefix+"%").Find(&chats).Error; err != nil {
		slog.Warn("resume queued nodes: chat scan failed", "component", "store", "err", err)
		return
	}
	for _, c := range chats {
		for _, n := range s.queuedFrontier(ctx, c.ID) {
			if resumable != nil {
				if ok, why := resumable(c.ID, string(dag.PauseShutdown)); !ok {
					rep.Failed = append(rep.Failed, s.FailUnresumable(ctx, c.ID, n, why))
					continue
				}
			}
			if err := s.armQueued(ctx, n); err != nil {
				slog.Warn("resume queued nodes: re-stamp failed", "component", "store", "plan", n.PlanID, "node", n.NodeID, "err", err)
				continue
			}
			s.syncDagNodeRecord(ctx, c.ID, n.PlanID, n.NodeID, dag.StatusPaused)
			rep.add(ResumableNode{ChatID: c.ID, PlanID: n.PlanID, NodeID: n.NodeID, Reason: dag.PauseShutdown}, string(dag.StatusPaused))
		}
	}
}

// queuedFrontier lists chatID's latest-plan nodes that are queued (no row, or waiting on admission) with every
// dependency done. Empty unless this instance owns every row: a live peer holding any of them holds the run.
func (s *Store) queuedFrontier(ctx context.Context, chatID string) []DagNode {
	p, err := s.GetLatestDagPlan(ctx, chatID)
	if err != nil || p == nil {
		return nil
	}
	plan, ok := s.LoadExecPlan(ctx, p.ID)
	if !ok {
		return nil
	}
	var rows []DagNode
	if err := s.db.WithContext(ctx).Where("plan_id = ?", p.ID).Find(&rows).Error; err != nil {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	status := map[string]string{}
	for _, r := range rows {
		if !s.ownsNodeRow(r) {
			return nil
		}
		status[r.NodeID] = r.Status
	}
	var out []DagNode
	for _, n := range plan.Nodes {
		if st, ok := status[n.ID]; ok && st != string(dag.StatusQueued) {
			continue
		}
		if allDone(n.DependsOn, status) {
			out = append(out, DagNode{PlanID: p.ID, NodeID: n.ID, Status: string(dag.StatusQueued)})
		}
	}
	return out
}

func allDone(ids []string, status map[string]string) bool {
	for _, id := range ids {
		if status[id] != string(dag.StatusDone) {
			return false
		}
	}
	return true
}

// ownsNodeRow mirrors ResumePausedDagNodes' ownership filter for one row.
func (s *Store) ownsNodeRow(n DagNode) bool {
	return n.InstanceID == "" || n.InstanceID == s.instanceID ||
		n.UpdatedAt.Before(time.Now().UTC().Add(-staleNodeCeiling))
}

// armQueued stamps a queued node paused/shutdown, creating its row if it never started, so its retry continues it.
// No client transition pauses a queued node, so an existing row moves by a CAS from queued, not CanTransition.
func (s *Store) armQueued(ctx context.Context, n DagNode) error {
	prev, err := s.GetDagNode(ctx, n.PlanID, n.NodeID)
	if err != nil {
		return err
	}
	if prev != nil {
		return s.casNodeStatus(ctx, n.PlanID, n.NodeID, dag.StatusQueued, dag.StatusPaused,
			map[string]any{"pause_reason": string(dag.PauseShutdown), "pending_question": "", "instance_id": s.instanceID})
	}
	n.Status, n.InstanceID = string(dag.StatusPaused), s.instanceID
	if err := s.UpsertDagNode(ctx, n); err != nil {
		return err
	}
	return s.SetNodeStatus(ctx, n.PlanID, n.NodeID, "", dag.PauseShutdown, "")
}
