// Package a2ui validates, merges and stores A2UI v0.9.1 surfaces for render_ui.
// schema/ embeds upstream github.com/google/A2UI schemas (Apache-2.0) plus quack's catalog.
package a2ui

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"regexp"
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

// QuizAnswer is one question's correct option. Label is the identity (option
// order and values move on a shuffle); Answer is its value on the current surface.
type QuizAnswer struct {
	Answer string `json:"answer"`
	Label  string `json:"label,omitempty"`
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
	return v.SurfaceID, CheckSurfaceID(v.SurfaceID)
}

var surfaceIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CheckSurfaceID: the id becomes part of an artifact file name and a UI key.
func CheckSurfaceID(id string) error {
	if !surfaceIDRe.MatchString(id) {
		return fmt.Errorf("surface_id %q must match %s", id, surfaceIDRe)
	}
	return nil
}

// Apply upserts comps onto s and keyIn onto key by question id, shuffles every
// graded incoming ChoicePicker, validates, then re-derives each answer's value
// from its label on the merged surface. Returns the merged key.
func Apply(s *Surface, key map[string]QuizAnswer, comps []Component, dataModel map[string]any, keyIn map[string]QuizAnswer) (map[string]QuizAnswer, error) {
	merged := maps.Clone(key)
	if merged == nil {
		merged = map[string]QuizAnswer{}
	}
	for _, q := range slices.Sorted(maps.Keys(keyIn)) {
		a, err := labelAnswer(q, keyIn[q], comps, s.Components)
		if err != nil {
			return nil, err
		}
		merged[q] = a
	}
	ShuffleQuiz(s.SurfaceID, comps, slices.Collect(maps.Keys(merged)))
	var err error
	if s.Components, err = Merge(s.Components, comps); err != nil {
		return nil, err
	}
	if dataModel != nil {
		s.DataModel = dataModel
	}
	if err := Validate(*s); err != nil {
		return nil, err
	}
	for _, q := range slices.Sorted(maps.Keys(merged)) {
		a := merged[q]
		c := pickerFor(s.Components, q)
		value, ok := optionField(c, "label", a.Label, "value")
		if c == nil || !ok {
			return nil, fmt.Errorf("answer_key[%q]: option %q is no longer on the surface's ChoicePicker for %q; resend answer_key for %q", q, a.Label, q, q)
		}
		a.Answer = value
		merged[q] = a
	}
	return merged, nil
}

// labelAnswer resolves a new key entry's answer value to its option label,
// on the incoming picker when one was sent, else the stored one.
func labelAnswer(q string, a QuizAnswer, incoming, stored []Component) (QuizAnswer, error) {
	c := pickerFor(incoming, q)
	if c == nil {
		c = pickerFor(stored, q)
	}
	if c == nil {
		return a, fmt.Errorf("answer_key[%q]: no ChoicePicker has id %q or a value path ending in /%s", q, q, q)
	}
	label, ok := optionField(c, "value", a.Answer, "label")
	if !ok {
		return a, fmt.Errorf("answer_key[%q]: answer %q is not one of ChoicePicker %q's option values %q", q, a.Answer, idOf(c), optionValues(c))
	}
	a.Label = label
	return a, nil
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

// ShuffleQuiz reorders the labels of every graded ChoicePicker in comps as a pure function of
// (surfaceID, picker id, label set), so a re-send in any order lands the same; values stay positional.
func ShuffleQuiz(surfaceID string, comps []Component, graded []string) {
	for _, q := range graded {
		c := pickerFor(comps, q)
		opts, ok := pickerOptions(c)
		if !ok {
			continue // malformed options; Validate reports them
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(surfaceID + "\x00" + idOf(c)))
		canon := slices.SortedFunc(slices.Values(opts), func(a, b map[string]any) int { return strings.Compare(text(a["label"]), text(b["label"])) })
		perm := rand.New(rand.NewPCG(h.Sum64(), 0)).Perm(len(opts))
		shuffled := make([]any, len(opts))
		for i, p := range perm {
			o := maps.Clone(canon[p])
			o["value"] = opts[i]["value"]
			shuffled[i] = o
		}
		c["options"] = shuffled
	}
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

func pickerOptions(c Component) ([]map[string]any, bool) {
	raw, _ := c["options"].([]any)
	opts := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		o, ok := r.(map[string]any)
		if _, isStr := o["value"].(string); !ok || !isStr {
			return nil, false
		}
		opts = append(opts, o)
	}
	return opts, len(opts) > 0
}

// optionField returns field `want` of c's first option whose field `by` reads as match.
func optionField(c Component, by, match, want string) (string, bool) {
	opts, _ := pickerOptions(c)
	for _, o := range opts {
		if text(o[by]) == match {
			return text(o[want]), true
		}
	}
	return "", false
}

func optionValues(c Component) []string {
	opts, _ := pickerOptions(c)
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = text(o["value"])
	}
	return out
}

// text is a string as-is and anything else (a bound label) as compact JSON, so it can serve as an identity.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func idOf(c Component) string {
	id, _ := c["id"].(string)
	return id
}
