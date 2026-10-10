package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

const kindDagPlan = "dag_plan"

// Assignment is one bit of work in the scope of a node's job: a task, the A2A referenceTaskIds it
// waits on (DependsOn; results travel as their artifacts), and the deterministic checks it runs.
type Assignment struct {
	NodeID    string   `json:"node_id"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
	Checks    []string `json:"checks,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
	Rubric    string   `json:"rubric,omitempty"`
	// TaskID: the A2A task_id this assignment dispatched as, recorded once execute runs it.
	TaskID string `json:"task_id,omitempty"`
	Result string `json:"result,omitempty"`
	// Meta: per-extension namespaced context (meta["github"] = {base_sha, ...}) written only by an
	// extension's SDK hook at plan creation, never by the model; execute's freshness check reads it.
	Meta map[string]map[string]any `json:"meta,omitempty"`
	// ForkOf: reserved; accepted and persisted, not yet written or interpreted by anything.
	ForkOf string `json:"fork_of,omitempty"`
	// Stopped: the user stopped this node, so Result is an unreviewed draft - never an answer,
	// in any later turn either (the executor's own stop flag is per turn).
	Stopped bool `json:"stopped,omitempty"`
}

// Status is the assignment's outcome in ApplyAssignmentOutcome's terms, "" when it has not run.
func (a Assignment) Status() string {
	switch {
	case a.TaskID == "":
		return ""
	case a.Stopped:
		return "cancelled"
	case strings.TrimSpace(a.Result) == "":
		return "failed"
	}
	return "done"
}

// DagPlanRecord is the "dag_plan" kind's body. Assignment.NodeID references a dag_node record; whether
// that node exists and is idle needs a live lookup no Validate closure can make, so the tool layer checks it.
type DagPlanRecord struct {
	PlanID      string       `json:"plan_id"`
	Assignments []Assignment `json:"assignments"`
	Setup       *Setup       `json:"setup,omitempty"`
	Delivery    *Delivery    `json:"delivery,omitempty"`
	Status      string       `json:"status,omitempty"`
	// Sinks: the latest step's sinks, whose outputs are its turn's answer; a retry or resume delivers exactly these.
	Sinks []string `json:"sinks,omitempty"`
}

const dagPlanJSONSchema = `{
  "type": "object",
  "required": ["plan_id", "assignments"],
  "properties": {
    "plan_id": {"type": "string"},
    "assignments": {"type": "array", "items": {"type": "object", "required": ["node_id", "task"], "properties": {
      "node_id": {"type": "string"}, "task": {"type": "string"},
      "depends_on": {"type": "array", "items": {"type": "string"}},
      "checks": {"type": "array", "items": {"type": "string"}},
      "workdir": {"type": "string"}, "rubric": {"type": "string"},
      "task_id": {"type": "string"}, "result": {"type": "string"}, "meta": {"type": "object"},
      "fork_of": {"type": "string"}, "stopped": {"type": "boolean"}
    }}},
    "setup": {"type": "object"},
    "delivery": {"type": "object"},
    "status": {"type": "string"},
    "sinks": {"type": "array", "items": {"type": "string"}}
  }
}`

func init() {
	recordstore.Register(kindDagPlan, recordstore.KindSpec{
		Class:      recordstore.Structured,
		JSONSchema: dagPlanJSONSchema,
		Validate:   validateDagPlanRecord,
		// Single instance per chat: every plan a chat builds is one id's revision history.
		Identity: func(_ []byte, _ string) (string, error) { return "main", nil },
		// AgentWritable false: only create_plan/edit_plan author a plan (node minting, live-node checks);
		// a bare write_dag_plan would bypass both.
		AgentWritable: false,
	})
}

// validateDagPlanRecord checks empty tasks, unknown or cyclic depends_on, and disallowed checks, each a
// one-line error naming the field and the fix. Node existence is the tool layer's job.
func validateDagPlanRecord(raw json.RawMessage) error {
	var rec DagPlanRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	if len(rec.Assignments) == 0 {
		return errors.New("assignments: at least one assignment is required")
	}
	known := make(map[string]bool, len(rec.Assignments))
	var ids []string
	for _, a := range rec.Assignments {
		if a.NodeID == "" {
			return errors.New("assignments: node_id is required")
		}
		if known[a.NodeID] {
			return fmt.Errorf("assignments: duplicate node_id %q", a.NodeID)
		}
		known[a.NodeID] = true
		ids = append(ids, a.NodeID)
	}
	sort.Strings(ids)
	nodes := make([]Node, 0, len(rec.Assignments))
	for _, a := range rec.Assignments {
		if err := ValidateTask(a.Task); err != nil {
			return fmt.Errorf("assignments.%s.%w", a.NodeID, err)
		}
		for _, dep := range a.DependsOn {
			if !known[dep] {
				return fmt.Errorf("assignments.%s.depends_on: unknown node id %q; this plan's node ids: %s",
					a.NodeID, dep, strings.Join(ids, ", "))
			}
		}
		if len(a.Checks) > 0 {
			if err := ValidateChecks(a.Checks); err != nil {
				return fmt.Errorf("assignments.%s.checks: %w", a.NodeID, err)
			}
		}
		nodes = append(nodes, Node{ID: a.NodeID, DependsOn: a.DependsOn})
	}
	if _, err := topoLayers(Plan{Nodes: nodes}); err != nil {
		return fmt.Errorf("assignments: %w", err)
	}
	if err := validateDelivery(rec.Delivery); err != nil {
		return err // already prefixed "delivery.kind: " - wrapping again would double it
	}
	return nil
}

// SaveDagPlanRecord writes rec as this chat's next dag_plan revision. nil artifacts = no service
// configured (fail-open); the caller Warn-logs and moves on.
func SaveDagPlanRecord(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, turnID string, rec DagPlanRecord) (id string, revision int, err error) {
	if artifacts == nil || chatID == "" {
		return "", 0, errors.New("dag: no artifact service configured")
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	lineage := recordstore.Lineage{NodeID: "planner", Author: "worker", TurnID: turnID}
	return c.SaveStructured(ctx, kindDagPlan, rec, "", lineage)
}

// LoadDagPlanRecord reads the chat's current dag_plan record; ok is false when none was ever created.
func LoadDagPlanRecord(ctx context.Context, artifacts artifact.Service, appName, userID, chatID string) (rec DagPlanRecord, revision int, ok bool, err error) {
	if artifacts == nil || chatID == "" {
		return DagPlanRecord{}, 0, false, nil
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	raw, rev, ok, err := c.Latest(ctx, kindDagPlan+":main")
	if err != nil || !ok {
		return DagPlanRecord{}, 0, ok, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return DagPlanRecord{}, 0, false, fmt.Errorf("dag_plan: stored content doesn't unmarshal: %w", err)
	}
	return rec, rev, true, nil
}
