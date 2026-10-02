package a2ui

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// TestPRTutorPromptExample keeps the pr-tutor prompt's worked example a surface render_ui accepts.
func TestPRTutorPromptExample(t *testing.T) {
	b, err := os.ReadFile("../../agents/pr-tutor/prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)`components`:\n\n```json\n(.*?)\n```.*?`data_model`:\n\n```json\n(.*?)\n```.*?`answer_key`:\n\n```json\n(.*?)\n```").FindSubmatch(b)
	if m == nil {
		t.Fatal("worked example not found in prompt.md")
	}
	sid := regexp.MustCompile("`surface_id`: \"([^\"]+)\"").FindSubmatch(b)
	if sid == nil {
		t.Fatal("worked example surface_id not found in prompt.md")
	}
	var comps []Component
	var model map[string]any
	var key map[string]QuizAnswer
	for i, dst := range []any{&comps, &model, &key} {
		if err := json.Unmarshal(m[i+1], dst); err != nil {
			t.Fatal(err)
		}
	}
	s := Surface{SurfaceID: string(sid[1]), CatalogID: CatalogID}
	if _, err := Apply(&s, nil, comps, model, key); err != nil {
		t.Fatalf("prompt example does not validate: %v", err)
	}
}
