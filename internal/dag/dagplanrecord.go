// dagplanrecord.go: the "dag_plan" artifact kind (#1090 P8, issue #1095).
// One accepted plan writes one revision at id "dag_plan:main" - there is no
// plan-judge revision loop in this codebase (the plan tool validates and
// caches synchronously; execute runs it as-is), so "one revision per
// accepted plan" is correct today, not a corner cut.
package dag

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

const kindDagPlan = "dag_plan"

// dagPlanJSONSchema mirrors Plan's shape (literal text, reviewed on its own -
// same convention as vetting's codeReviewJSONSchema). Fields are unmarshaled
// with Go's default (capitalized) names: Plan has no json tags.
const dagPlanJSONSchema = `{
  "type": "object",
  "required": ["ID", "Nodes"],
  "properties": {
    "ID": {"type": "string"},
    "Nodes": {"type": "array", "items": {"type": "object", "properties": {
      "ID": {"type": "string"}, "AgentName": {"type": "string"}, "Task": {"type": "string"},
      "DependsOn": {"type": "array", "items": {"type": "string"}}
    }}},
    "UserMessage": {"type": "string"},
    "Setup": {"type": "object"},
    "Delivery": {"type": "object"},
    "PlanOnly": {"type": "boolean"}
  }
}`

func init() {
	recordstore.Register(kindDagPlan, recordstore.KindSpec{
		Class:      recordstore.Structured,
		JSONSchema: dagPlanJSONSchema,
		Validate:   func(raw json.RawMessage) error { var p Plan; return json.Unmarshal(raw, &p) },
		// Single instance per chat: the chat is already the store's scoping
		// dimension (session=chatID), so every accepted plan in a chat is one
		// id's revision history, not a per-plan-id fan-out.
		Identity: func(_ []byte, _ string) (string, error) { return "main", nil },
	})
}

// SaveDagPlanRecord writes p as this chat's next dag_plan revision, fail-open
// like every other episodic write in #1090 (a save error never blocks
// execution - it's Warn-logged by the caller-shared recordClient pattern).
// artifacts nil is the pre-#1090 case (no artifact service configured).
func SaveDagPlanRecord(ctx context.Context, artifacts artifact.Service, appName, userID, chatID, turnID string, p Plan) (id string, revision int, err error) {
	if artifacts == nil || chatID == "" {
		return "", 0, errors.New("dag: no artifact service configured")
	}
	c := recordstore.New(artifacts, appName, userID, chatID)
	lineage := recordstore.Lineage{NodeID: "planner", Author: "worker", TurnID: turnID}
	return c.SaveStructured(ctx, kindDagPlan, p, "", lineage)
}
