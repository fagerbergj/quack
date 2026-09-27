package a2ui

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/recordstore"
)

type fixture struct {
	SurfaceID  string                `json:"surface_id"`
	Components []Component           `json:"components"`
	DataModel  map[string]any        `json:"data_model"`
	AnswerKey  map[string]QuizAnswer `json:"answer_key"`
}

func loadExample(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/example.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) surface() Surface {
	return Surface{SurfaceID: f.SurfaceID, CatalogID: CatalogID, Components: f.Components, DataModel: f.DataModel}
}

func comp(t *testing.T, cs []Component, id string) Component {
	t.Helper()
	for _, c := range cs {
		if c["id"] == id {
			return c
		}
	}
	t.Fatalf("no component %q", id)
	return nil
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, s *Surface)
		want   string // "" = valid
	}{
		{"spike example", func(*testing.T, *Surface) {}, ""},
		{"no data model", func(_ *testing.T, s *Surface) { s.DataModel = nil }, ""},
		{"dangling ref", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "quiz")
			c["children"] = append(c["children"].([]any), "nope")
		}, `component "quiz" references missing component "nope"`},
		{"dangling tab child", func(t *testing.T, s *Surface) {
			comp(t, s.Components, "tabs")["tabs"].([]any)[0].(map[string]any)["child"] = "gone"
		}, `references missing component "gone"`},
		{"missing root", func(t *testing.T, s *Surface) { comp(t, s.Components, "root")["id"] = "top" }, `no component has id "root"`},
		{"duplicate id", func(t *testing.T, s *Surface) {
			s.Components = append(s.Components, Component{"id": "title", "component": "Text", "text": "again"})
		}, `duplicate component id "title"`},
		{"cycle", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "quiz")
			c["children"] = append(c["children"].([]any), "main")
		}, "reference cycle"},
		{"unknown component", func(t *testing.T, s *Surface) { comp(t, s.Components, "flow")["component"] = "Graph" }, `unknown component "Graph"`},
		{"unknown property", func(t *testing.T, s *Surface) { comp(t, s.Components, "submit")["label"] = "Go" }, `unknown properties ["label"]`},
		{"mermaid without code", func(t *testing.T, s *Surface) { delete(comp(t, s.Components, "flow"), "code") }, `missing properties: ["code"]`},
		{"code bad startLine", func(t *testing.T, s *Surface) { comp(t, s.Components, "c1_code")["startLine"] = "48" }, `property "startLine"`},
		{"code without path", func(t *testing.T, s *Surface) { delete(comp(t, s.Components, "c1_code"), "path") }, `missing properties: ["path"]`},
		{"bad binding", func(t *testing.T, s *Surface) { comp(t, s.Components, "q1")["value"] = map[string]any{"path": 3} }, `property "value" does not match DynamicStringList`},
		{"bad surface id", func(_ *testing.T, s *Surface) { s.SurfaceID = "a/b" }, "surface_id"},
		{"wrong catalog", func(_ *testing.T, s *Surface) { s.CatalogID = "basic" }, "catalog_id"},
		{"empty", func(_ *testing.T, s *Surface) { s.Components = nil }, `no component has id "root"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := loadExample(t).surface()
			tc.mutate(t, &s)
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

// TestValidateSpikeSurfaces: the six surfaces qwen3.8-27b authored through the
// flat render_ui contract in the spike; each must pass with its key shuffled in.
func TestValidateSpikeSurfaces(t *testing.T) {
	letters := map[string]int{}
	f, err := os.Open("testdata/spike_args.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	n := 0
	for sc.Scan() {
		var fx fixture
		if err := json.Unmarshal(sc.Bytes(), &fx); err != nil {
			t.Fatal(err)
		}
		s := Surface{SurfaceID: fx.SurfaceID, CatalogID: CatalogID}
		if err := Apply(&s, fx.Components, fx.DataModel, fx.AnswerKey); err != nil {
			t.Errorf("%s: %v", fx.SurfaceID, err)
		}
		for _, a := range fx.AnswerKey {
			letters[a.Answer]++
		}
		n++
	}
	if n != 6 {
		t.Fatalf("read %d fixtures, want 6", n)
	}
	// Unshuffled, 16 of these 26 answers are "b".
	if letters["b"] > 10 {
		t.Errorf("answer letters after shuffle = %v; \"b\" still dominates", letters)
	}
}

func TestMerge(t *testing.T) {
	base := []Component{{"id": "root", "component": "Column", "children": []any{"a"}}, {"id": "a", "component": "Text", "text": "old"}}
	got, err := Merge(base, []Component{{"id": "a", "component": "Text", "text": "new"}, {"id": "b", "component": "Text", "text": "added"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, idOf(c))
	}
	if strings.Join(ids, ",") != "root,a,b" || got[1]["text"] != "new" {
		t.Fatalf("merge = %v", got)
	}
	if base[1]["text"] != "old" {
		t.Fatal("merge mutated base slice")
	}
	if _, err := Merge(nil, []Component{{"id": "x"}, {"id": "x"}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate in update: %v", err)
	}
	if _, err := Merge(nil, []Component{{"component": "Text"}}); err == nil || !strings.Contains(err.Error(), `no string "id"`) {
		t.Fatalf("missing id: %v", err)
	}
}

func TestShuffleQuiz(t *testing.T) {
	labelOf := func(c Component, value string) string {
		for _, o := range c["options"].([]any) {
			if m := o.(map[string]any); m["value"] == value {
				return m["label"].(string)
			}
		}
		return ""
	}
	run := func() (fixture, map[string]string) {
		f := loadExample(t)
		want := map[string]string{}
		for q, a := range f.AnswerKey {
			want[q] = labelOf(comp(t, f.Components, q), a.Answer)
		}
		if err := ShuffleQuiz(f.SurfaceID, f.Components, f.AnswerKey); err != nil {
			t.Fatal(err)
		}
		return f, want
	}
	f, want := run()
	for q, a := range f.AnswerKey {
		c := comp(t, f.Components, q)
		if got := labelOf(c, a.Answer); got != want[q] {
			t.Errorf("%s: key names %q, want label %q", q, got, want[q])
		}
		for i, o := range c["options"].([]any) {
			if v := o.(map[string]any)["value"]; v != string(rune('a'+i)) {
				t.Errorf("%s: option %d value %v; values must keep positional order", q, i, v)
			}
		}
	}
	again, _ := run()
	for q := range f.AnswerKey {
		if again.AnswerKey[q] != f.AnswerKey[q] {
			t.Errorf("%s: shuffle not deterministic", q)
		}
	}

	bad := loadExample(t)
	bad.AnswerKey["q1"] = QuizAnswer{Answer: "z"}
	if err := ShuffleQuiz(bad.SurfaceID, bad.Components, bad.AnswerKey); err == nil || !strings.Contains(err.Error(), `answer "z"`) {
		t.Fatalf("unknown answer: %v", err)
	}
}

func TestKindsRegistered(t *testing.T) {
	s := loadExample(t).surface()
	id, err := recordstore.IdentityFor(KindSurface, s, "")
	if err != nil || id != "a2ui_surface:pr-412-tutor" {
		t.Fatalf("surface id = %q, %v", id, err)
	}
	if id, err := recordstore.IdentityFor(KindQuizKey, QuizKey{SurfaceID: "pr-412-tutor"}, ""); err != nil || id != "quiz_key:pr-412-tutor" {
		t.Fatalf("quiz key id = %q, %v", id, err)
	}
	spec, _ := recordstore.SpecFor(KindSurface)
	raw, _ := json.Marshal(s)
	if err := spec.Validate(raw); err != nil {
		t.Fatalf("stored surface fails its kind validator: %v", err)
	}
	if err := spec.Validate([]byte(`{"surface_id":"x","catalog_id":"` + CatalogID + `","components":[]}`)); err == nil {
		t.Fatal("kind validator accepted a surface with no root")
	}
}
