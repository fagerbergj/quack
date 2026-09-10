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
// A2A contextId minted for this node when it's created - for a pi/ACP node
// it's whatever id that transport uses, stored in the same field. Slice 1
// only stores it; nothing reads it back yet (no cross-turn node reuse -
// that's slice 2).
type DagNodeRecord struct {
	NodeID    string     `json:"node_id"`
	Agent     string     `json:"agent"`
	Status    NodeStatus `json:"status,omitempty"`
	ContextID string     `json:"context_id,omitempty"`
}

const dagNodeJSONSchema = `{
  "type": "object",
  "required": ["node_id", "agent"],
  "properties": {
    "node_id": {"type": "string"},
    "agent": {"type": "string"},
    "status": {"type": "string"},
    "context_id": {"type": "string"}
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
	rec.Status = status
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
	_, _, err = c.SaveStructured(ctx, kindDagNode, rec, nodeID, lineage)
	return err
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
