// dag.Plan SSE/ledger helpers shared by execute and the HITL resume path.
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

func DagPlanEvent(ctx context.Context, p dag.Plan) stream.SSEEvent {
	nodes := make([]stream.DagNodeDef, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = stream.DagNodeDef{ID: n.ID, Agent: n.AgentName, Task: n.Task, DependsOn: n.DependsOn, ContextWindow: n.ContextWindow, Artifact: n.Artifact}
	}
	ev := stream.DagPlan(p.ID, nodes, planEdges(p.Nodes))
	if js, err := json.Marshal(p); err == nil {
		d := ev.Data.(stream.DagPlanData)
		d.ExecPlan = js
		ev.Data = d
	}
	return stream.WithTrace(ev, otelobs.TraceIDOf(ctx))
}

func planEdges(nodes []dag.Node) []stream.DagEdgeDef {
	var edges []stream.DagEdgeDef
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, stream.DagEdgeDef{From: dep, To: n.ID})
		}
	}
	return edges
}

// attachmentMeta: never the bytes - an oversized gen_ai.input.messages attribute is silently dropped.
type attachmentMeta struct {
	MIMEType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// summarizeAttachments is index-aligned; Bytes is 0 for artifactref (FileData) parts.
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

// genAIPlanStep: the dag_plan revision this execute saved - increasing, but not the Nth execute call
// (create_plan and every edit_plan bump it too).
const genAIPlanStep = "quack.plan.step"

// emitPlanEvent: step <= 0 (a store error) omits the step attribute.
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
	// The planner's actual ask as Build stamped it onto p, not a reconstruction from the plan.
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
