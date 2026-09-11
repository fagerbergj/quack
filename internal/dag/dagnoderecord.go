// dagnoderecord.go: the "dag_node" record kind - a node is someone doing an
// agent's job: id, agent, live status, A2A context.
// One record per minted node id, so the artifact panel can show who's on a
// plan without decoding assignment history. The plan maps onto A2A directly:
// a node's ContextID is the A2A contextId scoping every task it's dispatched
// (an assignment's own A2A task_id lives on the Assignment, not here).
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

// DagNodeRecord is the "dag_node" kind's structured body. ContextID is the
// A2A contextId minted for this node when it's created for a native node
// (stable for the node's life); an ACP/pi node overwrites it once its first
// round establishes a real transport session id (UpdateDagNodeContext), so a
// later reuse has something session/load can actually resume.
type DagNodeRecord struct {
	NodeID    string     `json:"node_id"`
	Agent     string     `json:"agent"`
	Status    NodeStatus `json:"status,omitempty"`
	ContextID string     `json:"context_id,omitempty"`
	// Started: true once this node has reached StatusRunning at least once -
	// the only reliable "a real session was ever created" signal shared by
	// both transports. A native node's ContextID never changes from its
	// mint-time placeholder (that's correct - the placeholder IS its real,
	// live session identity), so ContextID alone can't distinguish "native,
	// always resumable" from "ACP, failed/cancelled before ever
	// establishing a session" - Resumable() needs this bit precisely
	// because CanTransition allows queued -> failed/cancelled directly,
	// with no run in between.
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
		// Identity = the minted node id verbatim, stable across every status
		// update for this node.
		Identity:     func(_ []byte, hint string) (string, error) { return requireNodeHint(hint) },
		RequiresHint: true,
		// AgentWritable false: only create_plan/edit_plan mint a node (id
		// minting, the "currently running" check) - a bare write_dag_node would bypass both.
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
	return ValidateAgentName(rec.Agent)
}

// UpdateDagNodeStatus advances nodeID's persisted status to match the same
// lifecycle transition runlog.PersistNodeEvent mirrors onto the DagNode
// store row. ok=false when this chat has no dag_node record for nodeID (a
// config-bound workflow node, which never went through create_plan/
// edit_plan) - a silent no-op, fail-open like every other episodic write.
// Refuses an illegal transition against the record's OWN last-read status
// (CanTransition) rather than applying status unconditionally - this read
// is independent of runlog's own store-row check, so an interleaved
// cancel/done pair can't leave this record's mirror on a stale non-terminal
// status forever (list_nodes/nodeIsRunning both read this record, not the store row).
func UpdateDagNodeStatus(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, nodeID string, status NodeStatus) error {
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
	if !CanTransition(rec.Status, status) {
		return fmt.Errorf("dag_node %s: illegal status transition %s -> %s", nodeID, rec.Status, status)
	}
	rec.Status = status
	if status == StatusRunning {
		rec.Started = true
	}
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
	_, _, err = c.SaveStructured(ctx, kindDagNode, rec, nodeID, lineage)
	return err
}

// UpdateDagNodeContext overwrites nodeID's persisted ContextID - the ACP
// transport's real session id, learned only after its first round
// establishes one (dag/graph.go, at node completion). Same fail-open/no-op
// shape as UpdateDagNodeStatus; unlike status this has no transition table,
// a transport id is just replaced.
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

// Resumable reports whether list_nodes/create_plan-edit_plan reuse should
// offer this node for reassignment, and why: only a node that has actually
// finished a run (terminal status) AND actually started at some point has a
// session worth resuming - a node still queued has none yet, one
// running/paused is already live (nodeIsRunning already blocks reassigning
// those at plan-authoring time), and a node that went straight from queued
// to failed/cancelled (CanTransition allows this - an admission/setup
// failure, or a cancel before dispatch) never created one either.
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

// MintNodeID names the next node id for agent given every node id already
// minted this chat - "<agent>-<n>", one past the highest existing suffix
// for that agent. Never reused, even across an edit that later drops a
// node, so an old reference in history can never resolve to a fresh node's
// unrelated status/output.
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
