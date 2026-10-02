package a2ui

import (
	"fmt"
	"strings"
	"testing"
)

func diagramSurface() Surface {
	diagram := Component{
		"id": "root", "component": "Diagram", "direction": "LR",
		"layers": []any{
			map[string]any{"id": "ui", "title": "UI", "description": "What the user touches."},
			map[string]any{"id": "api", "title": "API", "description": "HTTP handlers."},
		},
		"nodes": []any{
			map[string]any{"id": "form", "label": "Form", "layer": "ui", "change": "added", "type": "feature", "detail": "New **form**."},
			map[string]any{"id": "handler", "label": "Handler", "layer": "api", "change": "modified", "detail": "Now validates."},
		},
		"edges": []any{
			map[string]any{"id": "submit", "from": "form", "to": "handler", "label": "POST", "detail": "Sends the form."},
		},
	}
	return Surface{SurfaceID: "d-1", CatalogID: CatalogID, Components: []Component{diagram}}
}

func TestValidateDiagram(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *Surface)
		want   string // "" = valid
	}{
		{"valid", func(*Surface) {}, ""},
		{"duplicate id across kinds", func(s *Surface) { s.Components[0]["edges"].([]any)[0].(map[string]any)["id"] = "form" }, `id "form" is used twice (node, then edge)`},
		{"node in unknown layer", func(s *Surface) { s.Components[0]["nodes"].([]any)[0].(map[string]any)["layer"] = "db" }, `node "form" is in layer "db"`},
		{"edge to unknown node", func(s *Surface) { s.Components[0]["edges"].([]any)[0].(map[string]any)["to"] = "ghost" }, `edge "submit" has to "ghost"`},
		{"edge from a layer", func(s *Surface) { s.Components[0]["edges"].([]any)[0].(map[string]any)["from"] = "ui" }, `edge "submit" has from "ui"`},
		{"blank node detail", func(s *Surface) { s.Components[0]["nodes"].([]any)[1].(map[string]any)["detail"] = "  " }, `node "handler" needs a non-empty detail`},
		{"blank edge detail", func(s *Surface) { s.Components[0]["edges"].([]any)[0].(map[string]any)["detail"] = "" }, `edge "submit" needs a non-empty detail`},
		{"blank layer description", func(s *Surface) { s.Components[0]["layers"].([]any)[0].(map[string]any)["description"] = "" }, `layer "ui" needs a non-empty description`},
		{"long node label", func(s *Surface) {
			s.Components[0]["nodes"].([]any)[0].(map[string]any)["label"] = strings.Repeat("x", 61)
		}, `node "form" label is 61 characters`},
		{"long layer title", func(s *Surface) {
			s.Components[0]["layers"].([]any)[0].(map[string]any)["title"] = strings.Repeat("t", maxLayerTitle+1)
		}, `layer "ui" title is 41 characters`},
		{"long edge label", func(s *Surface) {
			s.Components[0]["edges"].([]any)[0].(map[string]any)["label"] = strings.Repeat("e", maxEdgeLabel+1)
		}, `edge "submit" label is 41 characters`},
		{"too many edges", func(s *Surface) {
			var es []any
			for i := range maxEdges + 1 {
				es = append(es, map[string]any{"id": fmt.Sprintf("e%d", i), "from": "form", "to": "handler", "detail": "d"})
			}
			s.Components[0]["edges"] = es
		}, "exceed the limits"},
		{"empty layer", func(s *Surface) {
			s.Components[0]["layers"] = append(s.Components[0]["layers"].([]any), map[string]any{"id": "db", "title": "DB", "description": "d"})
		}, `layer "db" has no nodes`},
		{"too many nodes", func(s *Surface) {
			var ns []any
			for i := range maxNodes + 1 {
				ns = append(ns, map[string]any{"id": string(rune('A'+i/26)) + string(rune('a'+i%26)), "label": "n", "layer": "ui", "detail": "d"})
			}
			s.Components[0]["nodes"] = ns
		}, "exceed the limits"},
		{"bad change enum", func(s *Surface) { s.Components[0]["nodes"].([]any)[0].(map[string]any)["change"] = "renamed" }, `change`},
		{"missing detail key", func(s *Surface) { delete(s.Components[0]["nodes"].([]any)[0].(map[string]any), "detail") }, `missing properties`},
		{"unknown node key", func(s *Surface) { s.Components[0]["nodes"].([]any)[0].(map[string]any)["layerId"] = "ui" }, `nodes`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := diagramSurface()
			tc.mutate(&s)
			err := Validate(s)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateRiskTable(t *testing.T) {
	row := map[string]any{"change": "a.go", "type": "bugfix", "risk": "low", "reason": "One line.", "blast": "Called by x()."}
	s := Surface{SurfaceID: "r-1", CatalogID: CatalogID, Components: []Component{
		{"id": "root", "component": "RiskTable", "basis": "diff-only", "rows": []any{row}},
	}}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	row["risk"] = "critical"
	if err := Validate(s); err == nil || !strings.Contains(err.Error(), "risk") {
		t.Fatalf("want a risk enum error, got %v", err)
	}
}
