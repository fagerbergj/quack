package a2ui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxLayerTitle = 40
	maxNodeLabel  = 60
	maxEdgeLabel  = 40
	maxNodes      = 40
	maxEdges      = 80
)

// checkDiagram: what the catalog schema can't say about a Diagram - ids unique across layers,
// nodes and edges (explain_focus names an element by id alone), every reference resolves, every
// clickable element has text to show. Mermaid parsing is left to the frontend, which generates the source.
func checkDiagram(c Component) error {
	d := idOf(c)
	layers, nodes, edges := records(c["layers"]), records(c["nodes"]), records(c["edges"])
	if len(nodes) > maxNodes || len(edges) > maxEdges {
		return fmt.Errorf("Diagram %q: %d nodes and %d edges exceed the limits of %d and %d; show fewer, coarser elements", d, len(nodes), len(edges), maxNodes, maxEdges)
	}
	ids := map[string]string{}
	claim := func(kind string, r map[string]any) error {
		id, _ := r["id"].(string)
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("Diagram %q: a %s has an empty id", d, kind)
		}
		if prev, dup := ids[id]; dup {
			return fmt.Errorf("Diagram %q: id %q is used twice (%s, then %s); ids must be unique across layers, nodes and edges", d, id, prev, kind)
		}
		ids[id] = kind
		return nil
	}
	used := map[string]bool{}
	for _, l := range layers {
		if err := claim("layer", l); err != nil {
			return err
		}
		if err := checkText(d, "layer", l, "title", maxLayerTitle, true); err != nil {
			return err
		}
		if err := checkText(d, "layer", l, "description", 0, true); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		if err := claim("node", n); err != nil {
			return err
		}
		layer, _ := n["layer"].(string)
		if ids[layer] != "layer" {
			return fmt.Errorf("Diagram %q: node %q is in layer %q, which is not in layers; layers has %s", d, n["id"], layer, idList(layers))
		}
		used[layer] = true
		if err := checkText(d, "node", n, "label", maxNodeLabel, true); err != nil {
			return err
		}
		if err := checkText(d, "node", n, "detail", 0, true); err != nil {
			return err
		}
	}
	for _, l := range layers {
		if !used[l["id"].(string)] {
			return fmt.Errorf("Diagram %q: layer %q has no nodes; drop it or put a node in it", d, l["id"])
		}
	}
	for _, e := range edges {
		if err := checkEdge(d, e, ids, claim); err != nil {
			return err
		}
	}
	return nil
}

func checkEdge(d string, e map[string]any, ids map[string]string, claim func(string, map[string]any) error) error {
	if err := claim("edge", e); err != nil {
		return err
	}
	for _, end := range []string{"from", "to"} {
		ref, _ := e[end].(string)
		if ids[ref] != "node" {
			return fmt.Errorf("Diagram %q: edge %q has %s %q, which is not a node id", d, e["id"], end, ref)
		}
	}
	if err := checkText(d, "edge", e, "label", maxEdgeLabel, false); err != nil {
		return err
	}
	return checkText(d, "edge", e, "detail", 0, true)
}

// checkText bounds one string field of a record; max 0 means no length cap.
func checkText(d, kind string, r map[string]any, field string, max int, required bool) error {
	s, _ := r[field].(string)
	switch {
	case required && strings.TrimSpace(s) == "":
		return fmt.Errorf("Diagram %q: %s %q needs a non-empty %s", d, kind, r["id"], field)
	case max > 0 && utf8.RuneCountInString(s) > max:
		return fmt.Errorf("Diagram %q: %s %q %s is %d characters, over the limit of %d; shorten it", d, kind, r["id"], field, utf8.RuneCountInString(s), max)
	}
	return nil
}

func records(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func idList(rs []map[string]any) string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i], _ = r["id"].(string)
	}
	return fmt.Sprintf("%q", ids)
}
