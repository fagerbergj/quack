// plan.go: shared dag.Plan/SSE/ledger helpers used once a plan is actually
// running - by execute (after the plan judge accepts a dag_plan record) and
// by the HITL resume path. Authoring the plan itself now goes through
// list_nodes/create_plan/edit_plan (nodes.go/createplan.go/editplan.go),
// which build the dag_plan/dag_node records those tools eventually feed
// into the same dag.Plan shape via execute.go.
package tools

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// DagPlanEvent: builds the dag_plan SSE event. Shared by execute and the HITL resume path.
func DagPlanEvent(ctx context.Context, p dag.Plan) stream.SSEEvent {
	nodes := make([]stream.DagNodeDef, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = stream.DagNodeDef{ID: n.ID, Agent: n.AgentName, Task: n.Task, DependsOn: n.DependsOn, ContextWindow: n.ContextWindow, Artifact: n.Artifact}
	}
	return stream.WithTrace(stream.DagPlan(p.ID, nodes, planEdges(p.Nodes)), otelobs.TraceIDOf(ctx))
}

// planEdges: projects DependsOn into the wire edge list for the dag_plan event.
func planEdges(nodes []dag.Node) []stream.DagEdgeDef {
	var edges []stream.DagEdgeDef
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, stream.DagEdgeDef{From: dep, To: n.ID})
		}
	}
	return edges
}

// attachmentMeta is an input attachment's identifying shape, never its bytes -
// an oversized gen_ai.input.messages attribute gets silently dropped or
// truncated, which would lose the whole input field.
type attachmentMeta struct {
	MIMEType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// summarizeAttachments: strips a plan's attachments down to attachmentMeta,
// index-aligned. Attachments are artifactref reference parts (FileData) in
// practice - Bytes is 0 for those (size lives in the artifact service, not here).
func summarizeAttachments(parts []*genai.Part) []attachmentMeta {
	if len(parts) == 0 {
		return nil
	}
	out := make([]attachmentMeta, len(parts))
	for i, part := range parts {
		switch {
		case part == nil:
			continue
		case part.InlineData != nil:
			out[i] = attachmentMeta{MIMEType: part.InlineData.MIMEType, Bytes: len(part.InlineData.Data)}
		case part.FileData != nil:
			out[i] = attachmentMeta{MIMEType: part.FileData.MIMEType}
		}
	}
	return out
}

// genAIPlanStep: not a registered semconv attribute - the dag_plan revision
// THIS execute call saved (SaveStructured's own revision counter), monotonic
// per execute call but NOT a 1-based execute-step index: create_plan's own
// save is revision 1, and every edit_plan save between executes bumps it
// too, so e.g. create -> edit -> edit -> execute emits step=4 for the FIRST
// executed step. Still strictly increasing per execute, so ordering the
// ledger's per-turn "plan" events by it works - a future consumer just
// shouldn't assume step N means the Nth execute call.
const genAIPlanStep = "quack.plan.step"

// emitPlanEvent: records a gen_ai "plan" ledger event once execute has run
// one step of the current dag_plan record. step is the revision
// SaveStructured returned for it, <= 0 (a store error) to omit the attribute.
func emitPlanEvent(tc agent.Context, p *dag.Plan, step int) {
	if !otelobs.LoggingEnabled("quack.planner") {
		return
	}
	ctx := ledger.WithCoords(tc, ledger.Coords{ChatID: tc.SessionID(), Agent: "orchestrator", Round: "plan"})
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationPlan),
		attribute.String(otelobs.GenAIWorkflowName, p.ID),
	}
	if step > 0 {
		attrs = append(attrs, attribute.Int(genAIPlanStep, step))
	}
	// The planner's actual ask - history/message/attachments Build stamped onto p - not a
	// reconstruction from the plan it produced.
	if b, err := json.Marshal(struct {
		History     []dag.HistoryTurn `json:"history,omitempty"`
		Message     string            `json:"message,omitempty"`
		Attachments []attachmentMeta  `json:"attachments,omitempty"`
	}{p.History, p.UserMessage, summarizeAttachments(p.Attachments)}); err == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIInputMessages, string(b)))
	}
	if b, err := json.Marshal(p); err == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIOutputMessages, string(b)))
	}
	otelobs.EmitLog(ctx, "quack.planner", "", attrs...)
}
