package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/a2ui"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

type renderUIArgs struct {
	SurfaceID  string                     `json:"surface_id" jsonschema:"Stable id for the surface; reuse it to update the same surface."`
	Components []a2ui.Component           `json:"components" jsonschema:"Flat A2UI component objects (id, component, properties). Include root on first render; on an update send only added or changed components - they replace existing ones by id."`
	DataModel  map[string]any             `json:"data_model,omitempty" jsonschema:"Initial data model object; replaces the stored one when given."`
	AnswerKey  map[string]a2ui.QuizAnswer `json:"answer_key,omitempty" jsonschema:"Quiz answer key by question id: {answer, why}. Kept server-side, never rendered."`
}

type renderUIResult struct {
	ArtifactID string `json:"artifact_id"`
	Revision   int    `json:"revision"`
}

func newRenderUI(d Deps) (tool.Tool, error) {
	return NewRenderUITool(d.RecordStore, d.NodeID, d.Coords)
}

// NewRenderUITool builds render_ui: upserts an a2ui_surface artifact (plus its
// quiz_key when answer_key is given) and announces it with artifact_revision.
// A nil c builds fine but errors on a call.
func NewRenderUITool(c *recordstore.Client, nodeID string, coords *RoundCoords) (tool.Tool, error) {
	if coords == nil {
		coords = &RoundCoords{}
	}
	t, err := functiontool.New[renderUIArgs, string](
		functiontool.Config{
			Name: "render_ui",
			Description: "Render or update an A2UI v0.9.1 surface in the user's chat. Never write A2UI envelope messages; " +
				"pass the flat component list. Returns {\"artifact_id\",\"revision\"}, or a VALIDATION_FAILED: message " +
				"naming the first problem - fix it and call render_ui again.",
		},
		func(ctx agent.Context, a renderUIArgs) (string, error) {
			if c == nil {
				return "", errors.New("render_ui: no chat artifacts service configured")
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, TriggerAnnotation: coords.TriggerAnnotation, Author: "worker", SavedAt: time.Now().UTC()}
			return renderUI(ctx, c, lineage, a)
		},
	)
	if err != nil {
		return nil, err
	}
	return &decodeStringArgs{runnableTool: t.(runnableTool), keys: []string{"components", "data_model", "answer_key"}}, nil
}

func renderUI(ctx agent.Context, c *recordstore.Client, lineage recordstore.Lineage, a renderUIArgs) (string, error) {
	s := a2ui.Surface{SurfaceID: a.SurfaceID, CatalogID: a2ui.CatalogID}
	id, err := recordstore.IdentityFor(a2ui.KindSurface, s, "")
	if err != nil {
		return "VALIDATION_FAILED: " + err.Error(), nil
	}
	raw, _, found, err := c.Latest(ctx, id)
	if err != nil {
		return "", fmt.Errorf("render_ui: %w", err)
	}
	if found {
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("render_ui: stored %s: %w", id, err)
		}
	}
	if err := a2ui.Apply(&s, a.Components, a.DataModel, a.AnswerKey); err != nil {
		return "VALIDATION_FAILED: " + err.Error(), nil
	}
	id, rev, err := c.SaveStructured(ctx, a2ui.KindSurface, s, "", lineage)
	if err != nil {
		return "", fmt.Errorf("render_ui: %w", err)
	}
	emitRevision(ctx, id, rev, a2ui.KindSurface, lineage)
	if a.AnswerKey != nil {
		kid, krev, err := c.SaveStructured(ctx, a2ui.KindQuizKey, a2ui.QuizKey{SurfaceID: a.SurfaceID, Answers: a.AnswerKey}, "", lineage)
		if err != nil {
			return "", fmt.Errorf("render_ui: quiz key: %w", err)
		}
		emitRevision(ctx, kid, krev, a2ui.KindQuizKey, lineage)
	}
	b, err := json.Marshal(renderUIResult{ArtifactID: id, Revision: rev})
	return string(b), err
}

func emitRevision(ctx agent.Context, id string, rev int, kind string, l recordstore.Lineage) {
	if sink, ok := stream.YieldFromContext(ctx); ok {
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
