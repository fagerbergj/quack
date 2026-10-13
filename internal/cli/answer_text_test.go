package cli

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

// TestAnswerText_LabelsUnverified: an answer whose sink delivered after failing its quality check is labelled;
// a failed upstream node, or a sink that passed, leaves the answer plain.
func TestAnswerText_LabelsUnverified(t *testing.T) {
	const tmpl = `[{"type":"quack:dag","id":"d1","status":"completed","plan_id":"p1",
	  "nodes":[{"id":"research","agent":"web-researcher","task":"t","depends_on":[]},
	           {"id":"waivers","agent":"waiver-scout","task":"t","depends_on":["research"]}],
	  "edges":[{"from":"research","to":"waivers"}],
	  "node_states":{
	    "research":{"status":"done","output_preview":"r","judge_rounds":2,"judge_passed":false,"judge_final_score":0.1},
	    "waivers":{"status":"done","output_preview":"w","judge_rounds":2,"judge_passed":%s,"judge_final_score":0.333}}},
	 {"type":"message","id":"m1","status":"completed","content":[{"type":"output_text","text":"3 claims"}]}]`
	for _, tc := range []struct{ passed, want string }{
		{"false", "[Not verified (waivers: quality check score 0.33)]\n3 claims"},
		{"true", "3 claims"},
	} {
		var items []schema.OutputItem
		if err := json.Unmarshal([]byte(fmt.Sprintf(tmpl, tc.passed)), &items); err != nil {
			t.Fatal(err)
		}
		if got := answerText(items); got != tc.want {
			t.Errorf("judge_passed=%s: answerText = %q, want %q", tc.passed, got, tc.want)
		}
	}
}

func msgItem(t *testing.T, text string, stopped bool) []schema.OutputItem {
	t.Helper()
	var content []schema.ContentPart
	if text != "" {
		var cp schema.ContentPart
		if err := cp.FromOutputTextPart(schema.OutputTextPart{Text: text, Type: schema.OutputText}); err != nil {
			t.Fatal(err)
		}
		content = append(content, cp)
	}
	m := schema.MessageOutputItem{Id: "m", Type: "message", Status: schema.Completed, Content: content}
	if stopped {
		m.Stopped = &stopped
	}
	var it schema.OutputItem
	if err := it.FromMessageOutputItem(m); err != nil {
		t.Fatal(err)
	}
	return []schema.OutputItem{it}
}

// TestAnswerText_LabelsStoppedDraft: `chat show` and the transcript never print a stopped
// node's draft as a plain answer.
func TestAnswerText_LabelsStoppedDraft(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		stopped    bool
		want       string
	}{
		{"answer", "Rust 1.0 was 2015", false, "Rust 1.0 was 2015"},
		{"stopped draft", "Rust 1.0 was 2015", true, "[Stopped - not reviewed]\nRust 1.0 was 2015"},
		{"stopped, no draft", "", true, "[Stopped]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := answerText(msgItem(t, tc.text, tc.stopped)); got != tc.want {
				t.Errorf("answerText = %q, want %q", got, tc.want)
			}
		})
	}
}
