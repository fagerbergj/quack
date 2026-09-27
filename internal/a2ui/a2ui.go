// Package a2ui validates, merges and stores A2UI v0.9.1 surfaces for render_ui.
// schema/ embeds upstream github.com/google/A2UI schemas (Apache-2.0) plus quack's catalog.
package a2ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// CatalogID names quack's catalog: the v0.9.1 basic catalog plus Mermaid and Code.
const CatalogID = "https://quack.local/a2ui/v0_9/catalog.json"

// Artifact kinds render_ui writes; both are keyed by surface_id within a chat.
const (
	KindSurface = "a2ui_surface"
	KindQuizKey = "quiz_key"
)

// Component is one flat A2UI component object ({"id", "component", ...props}).
type Component = map[string]any

// Surface is the a2ui_surface artifact body: the full merged component list.
type Surface struct {
	SurfaceID  string         `json:"surface_id"`
	CatalogID  string         `json:"catalog_id"`
	Components []Component    `json:"components"`
	DataModel  map[string]any `json:"data_model,omitempty"`
}

// QuizKey is the quiz_key artifact body, linked to its surface by SurfaceID.
type QuizKey struct {
	SurfaceID string                `json:"surface_id"`
	Answers   map[string]QuizAnswer `json:"answers"`
}

// QuizAnswer is one question's correct option value and the reason for it.
type QuizAnswer struct {
	Answer string `json:"answer"`
	Why    string `json:"why,omitempty"`
}

func init() {
	// System: render_ui is the only writer; edit_artifact and write_<kind> would bypass validation and the shuffle.
	recordstore.Register(KindSurface, recordstore.KindSpec{
		Class: recordstore.Structured, SchemaVersion: 1, JSONSchema: `{"type":"object"}`,
		Validate: validateSurfaceJSON, Identity: surfaceIdentity, System: true,
	})
	recordstore.Register(KindQuizKey, recordstore.KindSpec{
		Class: recordstore.Structured, SchemaVersion: 1, JSONSchema: `{"type":"object"}`,
		Identity: surfaceIdentity, System: true,
	})
}

func validateSurfaceJSON(raw json.RawMessage) error {
	var s Surface
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	return Validate(s)
}

func surfaceIdentity(content []byte, _ string) (string, error) {
	var v struct {
		SurfaceID string `json:"surface_id"`
	}
	if err := json.Unmarshal(content, &v); err != nil {
		return "", err
	}
	return v.SurfaceID, checkSurfaceID(v.SurfaceID)
}

// checkSurfaceID: the id becomes an artifact file name, which ADK refuses with a slash.
func checkSurfaceID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("surface_id %q must be non-empty and contain no / or \\", id)
	}
	return nil
}

// Apply upserts comps onto s (and replaces its data model when dataModel is
// non-nil), shuffling the ChoicePickers key grades first, then validates the result.
func Apply(s *Surface, comps []Component, dataModel map[string]any, key map[string]QuizAnswer) error {
	if err := ShuffleQuiz(s.SurfaceID, comps, key); err != nil {
		return err
	}
	merged, err := Merge(s.Components, comps)
	if err != nil {
		return err
	}
	s.Components = merged
	if dataModel != nil {
		s.DataModel = dataModel
	}
	return Validate(*s)
}

// Merge upserts upd into base by id: a known id is replaced in place, a new id appended.
func Merge(base, upd []Component) ([]Component, error) {
	out := slices.Clone(base)
	at := make(map[string]int, len(out))
	for i, c := range out {
		at[idOf(c)] = i
	}
	seen := make(map[string]bool, len(upd))
	for i, c := range upd {
		id := idOf(c)
		if id == "" {
			return nil, fmt.Errorf("components[%d] has no string \"id\"", i)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate component id %q", id)
		}
		seen[id] = true
		if j, ok := at[id]; ok {
			out[j] = c
			continue
		}
		at[id] = len(out)
		out = append(out, c)
	}
	return out, nil
}

// ShuffleQuiz permutes the option labels of every ChoicePicker in comps that
// key grades (seeded by surfaceID) while values keep their positional order,
// and rewrites key so each answer still names the same label. Mutates comps and key.
func ShuffleQuiz(surfaceID string, comps []Component, key map[string]QuizAnswer) error {
	for _, q := range slices.Sorted(maps.Keys(key)) {
		c := pickerFor(comps, q)
		if c == nil {
			continue
		}
		opts, values, ok := pickerOptions(c)
		if !ok {
			continue // malformed options; Validate reports them
		}
		ans := key[q]
		if !slices.Contains(values, ans.Answer) {
			return fmt.Errorf("answer_key[%q]: answer %q is not one of ChoicePicker %q's option values %q", q, ans.Answer, idOf(c), values)
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(surfaceID + "\x00" + idOf(c)))
		perm := rand.New(rand.NewPCG(h.Sum64(), 0)).Perm(len(opts))
		shuffled := make([]any, len(opts))
		orig := ans.Answer
		for i, p := range perm {
			o := maps.Clone(opts[p])
			if values[p] == orig {
				ans.Answer = values[i]
			}
			o["value"] = values[i]
			shuffled[i] = o
		}
		c["options"] = shuffled
		key[q] = ans
	}
	return nil
}

// pickerFor finds the ChoicePicker graded by key entry q: id q, or bound to a path ending in /q.
func pickerFor(comps []Component, q string) Component {
	for _, c := range comps {
		if c["component"] != "ChoicePicker" {
			continue
		}
		binding, _ := c["value"].(map[string]any)
		path, _ := binding["path"].(string)
		if idOf(c) == q || strings.HasSuffix(path, "/"+q) {
			return c
		}
	}
	return nil
}

func pickerOptions(c Component) ([]map[string]any, []string, bool) {
	raw, _ := c["options"].([]any)
	opts := make([]map[string]any, 0, len(raw))
	values := make([]string, 0, len(raw))
	for _, r := range raw {
		o, ok := r.(map[string]any)
		v, isStr := o["value"].(string)
		if !ok || !isStr {
			return nil, nil, false
		}
		opts = append(opts, o)
		values = append(values, v)
	}
	return opts, values, len(opts) > 0
}

func idOf(c Component) string {
	id, _ := c["id"].(string)
	return id
}

// refs lists the component ids c points at, per the catalog's ComponentId/ChildList fields.
func refs(c Component) []string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	switch c["component"] {
	case "Card", "Button":
		add(c["child"])
	case "Modal":
		add(c["trigger"])
		add(c["content"])
	case "Row", "Column", "List":
		switch ch := c["children"].(type) {
		case []any:
			for _, v := range ch {
				add(v)
			}
		case map[string]any:
			add(ch["componentId"])
		}
	case "Tabs":
		tabs, _ := c["tabs"].([]any)
		for _, t := range tabs {
			if m, ok := t.(map[string]any); ok {
				add(m["child"])
			}
		}
	}
	return out
}

// checkIntegrity: unique ids, a root, no dangling references, and no cycles.
func checkIntegrity(comps []Component) error {
	byID := make(map[string]Component, len(comps))
	for _, c := range comps {
		id := idOf(c)
		if _, dup := byID[id]; dup {
			return fmt.Errorf("duplicate component id %q", id)
		}
		byID[id] = c
	}
	if _, ok := byID["root"]; !ok {
		return errors.New(`no component has id "root"`)
	}
	for _, c := range comps {
		for _, r := range refs(c) {
			if _, ok := byID[r]; !ok {
				return fmt.Errorf("component %q references missing component %q", idOf(c), r)
			}
		}
	}
	state := make(map[string]int, len(comps)) // 1 = on the DFS stack, 2 = done
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("component %q is its own ancestor (reference cycle)", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, r := range refs(byID[id]) {
			if err := visit(r); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, c := range comps {
		if err := visit(idOf(c)); err != nil {
			return err
		}
	}
	return nil
}
