package a2ui

import (
	"bufio"
	"encoding/json"
	"fmt"
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
			s.Components = append(s.Components, Component{"id": "x", "component": "Card", "child": "y"}, Component{"id": "y", "component": "Card", "child": "x"})
		}, `components ["x" "y"] are not reachable from root`},
		{"self reference", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "changes")
			c["children"] = append(c["children"].([]any), "changes")
		}, `"changes" references itself`},
		{"root has a parent", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "quiz")
			c["children"] = append(c["children"].([]any), "root")
		}, `references "root"`},
		{"orphan", func(_ *testing.T, s *Surface) {
			s.Components = append(s.Components, Component{"id": "stray", "component": "Text", "text": "x"})
		}, `components ["stray"] are not reachable`},
		{"duplicate option labels", func(t *testing.T, s *Surface) {
			comp(t, s.Components, "q1")["options"] = []any{map[string]any{"label": "same", "value": "a"}, map[string]any{"label": "same", "value": "b"}}
		}, "share the label"},
		{"unknown component", func(t *testing.T, s *Surface) { comp(t, s.Components, "flow")["component"] = "Graph" }, `unknown component "Graph"`},
		{"unknown property", func(t *testing.T, s *Surface) { comp(t, s.Components, "submit")["label"] = "Go" }, `unknown properties ["label"]`},
		{"mermaid without code", func(t *testing.T, s *Surface) { delete(comp(t, s.Components, "flow"), "code") }, `missing properties: ["code"]`},
		{"code bad startLine", func(t *testing.T, s *Surface) { comp(t, s.Components, "c1_code")["startLine"] = "48" }, `property "startLine"`},
		{"code without path", func(t *testing.T, s *Surface) { delete(comp(t, s.Components, "c1_code"), "path") }, `missing properties: ["path"]`},
		{"bad binding", func(t *testing.T, s *Surface) { comp(t, s.Components, "q1")["value"] = map[string]any{"path": 3} }, `property "value" must be an array of strings`},
		{"bad surface id", func(_ *testing.T, s *Surface) { s.SurfaceID = "a/b" }, "surface_id"},
		{"wrong catalog", func(_ *testing.T, s *Surface) { s.CatalogID = "basic" }, "catalog_id"},
		{"empty", func(_ *testing.T, s *Surface) { s.Components = nil }, `no component has id "root"`},
		{"surface id charset", func(_ *testing.T, s *Surface) { s.SurfaceID = "-x" }, "surface_id"},
		{"child listed twice", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "quiz")
			c["children"] = append(c["children"].([]any), "q1")
		}, `lists child "q1" twice`},
		{"two parents", func(t *testing.T, s *Surface) {
			c := comp(t, s.Components, "main")
			c["children"] = append(c["children"].([]any), "q1")
		}, `referenced by both`},
		{"empty options", func(t *testing.T, s *Surface) { comp(t, s.Components, "q1")["options"] = []any{} }, "at least one option"},
		{"javascript openUrl", func(t *testing.T, s *Surface) {
			comp(t, s.Components, "submit")["action"] = map[string]any{"functionCall": map[string]any{"call": "openUrl", "args": map[string]any{"url": "javascript:alert(1)"}}}
		}, "openUrl target"},
		{"https openUrl", func(t *testing.T, s *Surface) {
			comp(t, s.Components, "submit")["action"] = map[string]any{"functionCall": map[string]any{"call": "openUrl", "args": map[string]any{"url": "https://github.com/x"}}}
		}, ""},
		{"data image", func(t *testing.T, s *Surface) {
			s.Components = append(s.Components, Component{"id": "img", "component": "Image", "url": "data:image/png;base64,AA"})
			c := comp(t, s.Components, "main")
			c["children"] = append(c["children"].([]any), "img")
		}, "absolute http(s) URL"},
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
		key, err := Apply(&s, nil, fx.Components, fx.DataModel, fx.AnswerKey)
		if err != nil {
			t.Errorf("%s: %v", fx.SurfaceID, err)
		}
		for _, a := range key {
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

func labelOf(t *testing.T, c Component, value string) string {
	t.Helper()
	for _, o := range c["options"].([]any) {
		if m := o.(map[string]any); m["value"] == value {
			return m["label"].(string)
		}
	}
	return ""
}

func optionOrder(c Component) string {
	var labels []string
	for _, o := range c["options"].([]any) {
		labels = append(labels, o.(map[string]any)["label"].(string))
	}
	return strings.Join(labels, "|")
}

func TestShuffleQuiz(t *testing.T) {
	order := func(surfaceID string) map[string]string {
		f := loadExample(t)
		ShuffleQuiz(surfaceID, f.Components, []string{"q1", "q2", "q3"})
		out := map[string]string{}
		for _, q := range []string{"q1", "q2", "q3"} {
			c := comp(t, f.Components, q)
			out[q] = optionOrder(c)
			for i, o := range c["options"].([]any) {
				if v := o.(map[string]any)["value"]; v != string(rune('a'+i)) {
					t.Errorf("%s: option %d value %v; values must keep positional order", q, i, v)
				}
			}
		}
		return out
	}
	first, again := order("pr-412-tutor"), order("pr-412-tutor")
	if fmt.Sprint(first) != fmt.Sprint(again) {
		t.Fatalf("same seed, different order: %v vs %v", first, again)
	}
	// Re-sending in the displayed (shuffled) order must not re-permute.
	f := loadExample(t)
	ShuffleQuiz("pr-412-tutor", f.Components, []string{"q1"})
	shown := optionOrder(comp(t, f.Components, "q1"))
	ShuffleQuiz("pr-412-tutor", f.Components, []string{"q1"})
	if again := optionOrder(comp(t, f.Components, "q1")); again != shown {
		t.Fatalf("re-send in stored order moved options: %s -> %s", shown, again)
	}
	orig := loadExample(t)
	moved := false
	for _, sid := range []string{"pr-412-tutor", "other-surface", "third"} {
		for q, o := range order(sid) {
			moved = moved || o != optionOrder(comp(t, orig.Components, q))
		}
	}
	if !moved {
		t.Fatal("no seed moved any option; the shuffle is a no-op")
	}
}

// applyExample renders the example with its key, returning the surface and the merged key.
func applyExample(t *testing.T) (Surface, map[string]QuizAnswer) {
	t.Helper()
	f := loadExample(t)
	s := Surface{SurfaceID: f.SurfaceID, CatalogID: CatalogID}
	key, err := Apply(&s, nil, f.Components, f.DataModel, f.AnswerKey)
	if err != nil {
		t.Fatal(err)
	}
	return s, key
}

func TestApplyKeyFollowsLabels(t *testing.T) {
	s, key := applyExample(t)
	want := map[string]string{"q1": "It is marked dead immediately", "q2": "3s", "q3": "FOR UPDATE SKIP LOCKED on the claim query"}
	for q, label := range want {
		if key[q].Label != label || labelOf(t, comp(t, s.Components, q), key[q].Answer) != label {
			t.Fatalf("%s: key %+v does not point at %q", q, key[q], label)
		}
	}

	// Re-sending q1 in the model's original order, no answer_key: the key must follow the label.
	resent := loadExample(t)
	if _, err := Apply(&s, key, []Component{comp(t, resent.Components, "q1")}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, comp(t, s.Components, "q1"), key["q1"].Answer); got != want["q1"] {
		t.Fatalf("after re-send, key q1 -> %q, want %q", got, want["q1"])
	}
	key, err := Apply(&s, key, []Component{comp(t, resent.Components, "q1")}, nil, nil)
	if err != nil || labelOf(t, comp(t, s.Components, "q1"), key["q1"].Answer) != want["q1"] {
		t.Fatalf("after re-send: %v, key %+v", err, key["q1"])
	}

	// A picker whose correct label disappeared can't be graded any more.
	q1 := comp(t, loadExample(t).Components, "q1")
	q1["options"] = []any{map[string]any{"label": "new A", "value": "a"}, map[string]any{"label": "new B", "value": "b"}}
	if _, err := Apply(&s, key, []Component{q1}, nil, nil); err == nil || !strings.Contains(err.Error(), "resend answer_key") {
		t.Fatalf("vanished label: %v", err)
	}
}

func TestApplyLateKeyMerges(t *testing.T) {
	s, key := applyExample(t)
	q4 := Component{"id": "q4", "component": "ChoicePicker", "value": map[string]any{"path": "/answers/q4"},
		"options": []any{map[string]any{"label": "yes", "value": "a"}, map[string]any{"label": "no", "value": "b"}}}
	quiz := comp(t, s.Components, "quiz")
	quiz["children"] = []any{"q1", "q2", "q3", "q4", "submit"}
	merged, err := Apply(&s, key, []Component{q4, quiz}, nil, map[string]QuizAnswer{"q4": {Answer: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 4 || merged["q4"].Label != "no" || merged["q1"] != key["q1"] {
		t.Fatalf("late key merge = %+v", merged)
	}
	for name, in := range map[string]map[string]QuizAnswer{
		"unknown question": {"q9": {Answer: "a"}},
		"unknown answer":   {"q1": {Answer: "z"}},
	} {
		if _, err := Apply(&s, key, nil, nil, in); err == nil || !strings.Contains(err.Error(), "answer_key") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExplainAndTruncate(t *testing.T) {
	s := loadExample(t).surface()
	comp(t, s.Components, "title")["text"] = 5
	if err := Validate(s); err == nil || !strings.Contains(err.Error(), `must be a string, {"path"`) {
		t.Fatalf("DynamicString hint: %v", err)
	}
	if got := truncate(strings.Repeat("é", 10), 5); got != "éé..." {
		t.Fatalf("truncate split a rune: %q", got)
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

func TestHTTPURLPolicy(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/x": true, "http://example.com/a.png": true, "HTTP://EXAMPLE.COM/": true,
		"file:///etc/passwd": false, "//host/x.png": false, "/relative/x.png": false, "x.png": false,
		"ftp://host/x": false, "javascript:alert(1)": false, "data:image/png;base64,AA": false, "https:///nohost": false,
	} {
		if got := httpURL(u); got != want {
			t.Errorf("httpURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// TestApplyDuplicateLabelsRejected: a key resolves value->label->value, so twin labels would flip the stored answer.
func TestApplyDuplicateLabelsRejected(t *testing.T) {
	f := loadExample(t)
	comp(t, f.Components, "q1")["options"] = []any{map[string]any{"label": "same", "value": "a"}, map[string]any{"label": "same", "value": "b"}}
	s := Surface{SurfaceID: f.SurfaceID, CatalogID: CatalogID}
	if _, err := Apply(&s, nil, f.Components, f.DataModel, map[string]QuizAnswer{"q1": {Answer: "b"}}); err == nil || !strings.Contains(err.Error(), "share the label") {
		t.Fatalf("duplicate labels: %v", err)
	}
}
