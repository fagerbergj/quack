package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

const kindDagNode = "dag_node"

// DagNodeRecord is the "dag_node" kind's body: one per minted node id. ContextID is the A2A contextId
// for a native node; an ACP node overwrites it with its real transport session id (UpdateDagNodeContext).
type DagNodeRecord struct {
	NodeID    string     `json:"node_id"`
	Agent     string     `json:"agent"`
	Status    NodeStatus `json:"status,omitempty"`
	ContextID string     `json:"context_id,omitempty"`
	// Started: the node reached StatusRunning at least once. ContextID can't tell a native node from an ACP
	// node that failed before establishing a session (CanTransition allows queued -> failed), so Resumable needs it.
	Started bool `json:"started,omitempty"`
}

const dagNodeJSONSchema = `{
  "type": "object",
  "required": ["node_id", "agent"],
  "properties": {
    "node_id": {"type": "string"},
    "agent": {"type": "string"},
    "status": {"type": "string"},
    "context_id": {"type": "string"},
    "started": {"type": "boolean"}
  }
}`

func init() {
	recordstore.Register(kindDagNode, recordstore.KindSpec{
		Class:      recordstore.Structured,
		JSONSchema: dagNodeJSONSchema,
		Validate:   validateDagNode,
		// Identity = the minted node id verbatim, stable across every status update.
		Identity:     func(_ []byte, hint string) (string, error) { return requireNodeHint(hint) },
		RequiresHint: true,
		// AgentWritable false: only create_plan/edit_plan mint a node (id minting, the "currently running"
		// check); a bare write_dag_node would bypass both.
		AgentWritable: false,
	})
}

func requireNodeHint(hint string) (string, error) {
	if hint == "" {
		return "", errors.New("dag_node: no node id available for this record's instance")
	}
	return hint, nil
}

func validateDagNode(raw json.RawMessage) error {
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	if rec.NodeID == "" {
		return errors.New("node_id: must not be empty")
	}
	// Live, not just current: a node finishing on a pinned roster writes its status after a reload may
	// have removed its agent.
	return ValidateAgentNameIn(rec.Agent, liveAgentNames())
}

// UpdateDagNodeStatus mirrors a lifecycle transition onto nodeID's record; ok=false (fail-open) when none exists.
// It checks CanTransition against the record's own status, so an interleaved cancel/done can't leave it stale.
func UpdateDagNodeStatus(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, nodeID string, status NodeStatus) error {
	return updateDagNodeStatus(ctx, artifacts, appName, userID, chatID, nodeID, status, false)
}

// SyncDagNodeStatus is UpdateDagNodeStatus for boot's reconcile: no transition check (the row
// write it mirrors, e.g. paused -> failed, is unconditional) and no agent check (no roster yet).
func SyncDagNodeStatus(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, nodeID string, status NodeStatus) error {
	return updateDagNodeStatus(ctx, artifacts, appName, userID, chatID, nodeID, status, true)
}

func updateDagNodeStatus(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, nodeID string, status NodeStatus, force bool) error {
	if artifacts == nil || chatID == "" || nodeID == "" {
		return nil
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	raw, _, ok, err := c.Latest(ctx, kindDagNode+":"+nodeID)
	if err != nil || !ok {
		return err
	}
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("dag_node: stored content doesn't unmarshal: %w", err)
	}
	if rec.Status == status {
		return nil
	}
	if !force && !CanPersist(rec.Status, status) {
		return fmt.Errorf("dag_node %s: illegal status transition %s -> %s", nodeID, rec.Status, status)
	}
	rec.Status = status
	if status == StatusRunning {
		rec.Started = true
	}
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
	save := c.SaveStructured
	if force {
		save = c.ResaveStructured
	}
	_, _, err = save(ctx, kindDagNode, rec, nodeID, lineage)
	return err
}

// FailOpenDagNodeRecords marks every chat's dag_node record not yet done/failed/cancelled as failed:
// boot's settle for a run killed before it had any resumable state.
func FailOpenDagNodeRecords(ctx context.Context, artifacts artifact.Service, appName, userID, chatID string) error {
	return settleOpenDagNodeRecords(ctx, artifacts, appName, userID, chatID, StatusFailed, true)
}

// CancelUnstartedDagNodeRecords cancels the chat's queued (never run) dag_node records on a user stop
// before execute dispatched them; paused nodes are left to resume.
func CancelUnstartedDagNodeRecords(ctx context.Context, artifacts artifact.Service, appName, userID, chatID string) error {
	return settleOpenDagNodeRecords(ctx, artifacts, appName, userID, chatID, StatusCancelled, false)
}

// settleOpenDagNodeRecords moves open records to `to`; all=false leaves live and paused ones alone.
func settleOpenDagNodeRecords(ctx context.Context, artifacts artifact.Service, appName, userID, chatID string, to NodeStatus, all bool) error {
	if artifacts == nil {
		return nil
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	summaries, err := c.List(ctx, kindDagNode)
	if err != nil {
		return err
	}
	for _, s := range summaries {
		raw, _, ok, err := c.Latest(ctx, s.ID)
		var rec DagNodeRecord
		if err != nil || !ok || json.Unmarshal(raw, &rec) != nil {
			continue
		}
		switch rec.Status {
		case StatusDone, StatusFailed, StatusCancelled:
			continue
		case StatusPaused, StatusNeedsInput, StatusRunning:
			if !all {
				continue
			}
		}
		if err := SyncDagNodeStatus(ctx, artifacts, appName, userID, chatID, rec.NodeID, to); err != nil {
			return err
		}
	}
	return nil
}

// UpdateDagNodeContext replaces nodeID's ContextID with the ACP transport's real session id, learned
// after its first round. Same fail-open shape as UpdateDagNodeStatus, with no transition table.
func UpdateDagNodeContext(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, nodeID, contextID string) error {
	if artifacts == nil || chatID == "" || nodeID == "" || contextID == "" {
		return nil
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	raw, _, ok, err := c.Latest(ctx, kindDagNode+":"+nodeID)
	if err != nil || !ok {
		return err
	}
	var rec DagNodeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("dag_node: stored content doesn't unmarshal: %w", err)
	}
	if rec.ContextID == contextID {
		return nil
	}
	rec.ContextID = contextID
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
	_, _, err = c.SaveStructured(ctx, kindDagNode, rec, nodeID, lineage)
	return err
}

// Resumable reports whether reuse should offer this node, and why: only a terminal node that actually
// started has a session worth resuming (queued has none, running/paused is live).
func (r DagNodeRecord) Resumable() (bool, string) {
	switch r.Status {
	case StatusDone, StatusFailed, StatusCancelled:
		if !r.Started {
			return false, "no session recorded"
		}
		verb := map[NodeStatus]string{StatusDone: "done", StatusFailed: "failed", StatusCancelled: "cancelled"}[r.Status]
		return true, verb + " - continues its own session"
	case StatusRunning:
		return false, "currently running"
	case StatusPaused, StatusNeedsInput:
		return false, "paused - resolve or cancel it first"
	default:
		return false, "queued - hasn't run yet"
	}
}

// MintNodeID names the next "<agent>-<n>" id, one past the highest existing suffix for agent. Never
// reused, so an old reference can't resolve to an unrelated fresh node.
func MintNodeID(agent string, existing []string) string {
	max := 0
	prefix := agent + "-"
	for _, id := range existing {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		if n, err := strconv.Atoi(id[len(prefix):]); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s-%d", agent, max+1)
}
