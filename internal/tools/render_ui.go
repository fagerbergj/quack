package tools

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/a2ui"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

type renderUIArgs struct {
	SurfaceID  string                     `json:"surface_id" jsonschema:"Stable id for the surface ([A-Za-z0-9._-], starting alphanumeric); reuse it to update the same surface."`
	Components []a2ui.Component           `json:"components" jsonschema:"Flat A2UI component objects (id, component, properties). Include root on first render; on an update send only added or changed components - they replace existing ones by id."`
	DataModel  map[string]any             `json:"data_model,omitempty" jsonschema:"Initial data model object; replaces the stored one when given."`
	AnswerKey  map[string]a2ui.QuizAnswer `json:"answer_key,omitempty" jsonschema:"Quiz answer key by question id: {answer: option value, why}. Merged into the stored key by question id; kept server-side, never rendered."`
}

type renderUIResult struct {
	ArtifactID string `json:"artifact_id"`
	Revision   int    `json:"revision"`
}

func newRenderUI(d Deps) (tool.Tool, error) {
	return renderUITool(d.RecordStore, d.NodeID, d.Coords, d.Sink, d.TurnID)
}

// NewRenderUITool builds render_ui: upserts an a2ui_surface artifact (plus its
// quiz_key) and announces each save with artifact_revision. A nil c errors on a call.
func NewRenderUITool(c *recordstore.Client, nodeID string, coords *RoundCoords) (tool.Tool, error) {
	return renderUITool(c, nodeID, coords, nil, "")
}

// renderUITool: sink/turnID stand in when the call's ctx lacks them (a DAG node's worker, behind its A2A server).
func renderUITool(c *recordstore.Client, nodeID string, coords *RoundCoords, sink func(stream.SSEEvent), turnID string) (tool.Tool, error) {
	if coords == nil {
		coords = &RoundCoords{}
	}
	schema, err := jsonschema.For[renderUIArgs](nil)
	if err != nil {
		return nil, err
	}
	schema.Properties["components"].Types, schema.Properties["components"].Type = nil, "array"
	t, err := functiontool.New[renderUIArgs, string](
		functiontool.Config{
			Name: "render_ui",
			Description: "Render or update an A2UI v0.9.1 surface in the user's chat. Never write A2UI envelope messages; " +
				"pass the flat component list. answer_key entries merge into the surface's stored key by question id, so a " +
				"later call only sends the questions it adds or changes. Returns {\"artifact_id\",\"revision\"}, or a " +
				"VALIDATION_FAILED: message naming the first problem - fix it and call render_ui again.",
			InputSchema: schema,
		},
		func(ctx agent.Context, a renderUIArgs) (string, error) {
			if c == nil {
				return "", errors.New("render_ui: no chat artifacts service configured")
			}
			// The chat turn id, not coords.TurnID (a worker's ADK invocation id): the UI places a surface on its turn by it.
			turn := cmp.Or(stream.TurnIDFromContext(ctx), turnID)
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: turn, HeadSHA: coords.HeadSHA, TriggerAnnotation: coords.TriggerAnnotation, Author: "worker", SavedAt: time.Now().UTC()}
			emit := sink
			if s, ok := stream.YieldFromContext(ctx); ok {
				emit = s
			}
			return renderUI(ctx, c, lineage, emit, a)
		},
	)
	if err != nil {
		return nil, err
	}
	return &decodeStringArgs{runnableTool: t.(runnableTool), keys: []string{"components", "data_model", "answer_key"}}, nil
}

func renderUI(ctx agent.Context, c *recordstore.Client, lineage recordstore.Lineage, sink func(stream.SSEEvent), a renderUIArgs) (string, error) {
	if err := a2ui.CheckSurfaceID(a.SurfaceID); err != nil {
		return "VALIDATION_FAILED: " + err.Error(), nil
	}
	// Parallel calls in one model response run concurrently; this keeps each read-apply-save whole.
	defer c.LockKey("render_ui:" + a.SurfaceID)()
	s := a2ui.Surface{SurfaceID: a.SurfaceID, CatalogID: a2ui.CatalogID}
	key := a2ui.QuizKey{SurfaceID: a.SurfaceID}
	sid, _ := recordstore.IdentityFor(a2ui.KindSurface, s, "")
	kid, _ := recordstore.IdentityFor(a2ui.KindQuizKey, key, "")
	storedKey, err := loadJSON(ctx, c, kid, &key)
	if err != nil {
		return "", err
	}
	if _, err := loadJSON(ctx, c, sid, &s); err != nil {
		return "", err
	}
	merged, err := a2ui.Apply(&s, key.Answers, a.Components, a.DataModel, a.AnswerKey)
	if err != nil {
		return "VALIDATION_FAILED: " + err.Error(), nil
	}
	sid, rev, err := c.SaveStructured(ctx, a2ui.KindSurface, s, "", lineage)
	if err != nil {
		return "", fmt.Errorf("render_ui: %w", err)
	}
	emitRevision(sink, sid, rev, a2ui.KindSurface, lineage)
	key.Answers = merged
	if newKey, _ := json.Marshal(key); len(merged) > 0 && !bytes.Equal(newKey, storedKey) {
		kid, krev, err := c.SaveStructured(ctx, a2ui.KindQuizKey, key, "", lineage)
		if err != nil {
			return "", fmt.Errorf("render_ui: quiz key: %w", err)
		}
		emitRevision(sink, kid, krev, a2ui.KindQuizKey, lineage)
	}
	b, err := json.Marshal(renderUIResult{ArtifactID: sid, Revision: rev})
	return string(b), err
}

// loadJSON decodes id's latest revision into v, returning its raw bytes (nil when absent).
func loadJSON(ctx agent.Context, c *recordstore.Client, id string, v any) ([]byte, error) {
	raw, _, found, err := c.Latest(ctx, id)
	if err != nil || !found {
		return nil, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("render_ui: stored %s: %w", id, err)
	}
	return raw, nil
}

func emitRevision(sink func(stream.SSEEvent), id string, rev int, kind string, l recordstore.Lineage) {
	if sink != nil {
		sink(stream.SSEEvent{Name: stream.EventArtifactRevision, Data: stream.ArtifactRevisionData{ID: id, Revision: rev, Kind: kind, NodeID: l.NodeID, Round: l.Round}})
	}
}

// decodeStringArgs JSON-decodes string values under keys before the inner
// tool's schema check: models sometimes send a nested argument stringified.
type decodeStringArgs struct {
	runnableTool
	keys []string
}

func (d *decodeStringArgs) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return rebindToolMap(d.runnableTool, d, ctx, req)
}

func (d *decodeStringArgs) Run(ctx agent.Context, args any) (map[string]any, error) {
	if m, ok := args.(map[string]any); ok {
		m = maps.Clone(m)
		for _, k := range d.keys {
			if s, ok := m[k].(string); ok {
				var v any
				if json.Unmarshal([]byte(s), &v) == nil {
					m[k] = v
				}
			}
		}
		args = m
	}
	return d.runnableTool.Run(ctx, args)
}
