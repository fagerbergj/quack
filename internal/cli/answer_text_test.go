package cli

import (
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

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
