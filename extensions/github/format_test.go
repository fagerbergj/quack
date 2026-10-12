package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFooterTextVariants(t *testing.T) {
	tests := []struct {
		name               string
		version, publicURL string
		want               string
	}{
		{"both", "0.51.26", "https://quack.example.com", `<sub>quack 0.51.26 · <a href="https://quack.example.com/chat/chat1">run</a></sub>`},
		{"only version", "0.51.26", "", "<sub>quack 0.51.26</sub>"},
		{"only URL", "", "https://quack.example.com", `<sub>quack · <a href="https://quack.example.com/chat/chat1">run</a></sub>`},
		{"neither", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := footerText(tt.version, tt.publicURL, "chat1"); got != tt.want {
				t.Errorf("footerText(%q, %q, chat1) = %q, want %q", tt.version, tt.publicURL, got, tt.want)
			}
		})
	}
}

func TestWithFooterNeitherFieldSetLeavesBodyUnchanged(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := app.withFooter("LGTM", "chat1"); got != "LGTM" {
		t.Errorf("withFooter with no version/URL = %q, want body unchanged", got)
	}
}

func TestWithFooterIdempotent(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.SetFooter("0.51.26", "https://quack.example.com")
	once := app.withFooter("LGTM", "chat1")
	if !strings.Contains(once, "<sub>quack 0.51.26") {
		t.Fatalf("first render has no footer: %q", once)
	}
	twice := app.withFooter(once, "chat1")
	if twice != once {
		t.Errorf("re-rendering an already-footed body changed it: got %q, want %q", twice, once)
	}
	if n := strings.Count(twice, "<sub>quack"); n != 1 {
		t.Errorf("footer appears %d times, want 1: %q", n, twice)
	}
}

// The footer must stay the body's last line, with the outcome line slotted above it.
func TestAppendLineAfterFooter(t *testing.T) {
	withFooter := "LGTM\n\n<!-- quack:delivery:review:approve -->\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	got, changed := appendLine(withFooter, "Merged as abc1234.")
	if !changed {
		t.Fatal("expected the outcome line to be added")
	}
	want := "LGTM\n\n<!-- quack:delivery:review:approve -->\n\nMerged as abc1234.\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	if got != want {
		t.Errorf("appendLine after footer = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "</sub>") {
		t.Errorf("footer no longer last: %q", got)
	}
	// Idempotent even with the footer present.
	if again, changed := appendLine(got, "Merged as abc1234."); changed || again != got {
		t.Errorf("re-appending the same outcome line changed the body: %q, %v", again, changed)
	}
}

// The own-PR verdict lookup (reviewVerdictMarkerRe/reviewHeadMarkerRe) still finds its markers
// once withFooter's trailing <sub> block follows them.
func TestReviewVerdictMarkerFoundWithFooterPresent(t *testing.T) {
	body := "_Own PR: verdict approve._\n\n<!-- quack:delivery:review:approve -->\n<!-- quack:delivery:head:deadbeef -->\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	m := reviewVerdictMarkerRe.FindStringSubmatch(body)
	if m == nil || m[1] != "approve" {
		t.Fatalf("reviewVerdictMarkerRe found %v, want [approve]", m)
	}
	hm := reviewHeadMarkerRe.FindStringSubmatch(body)
	if hm == nil || hm[1] != "deadbeef" {
		t.Fatalf("reviewHeadMarkerRe found %v, want [deadbeef]", hm)
	}
}

// A posted review body keeps the delivery marker findable alongside the trailing footer.
func TestSubmitReviewCarriesMarkerAndFooter(t *testing.T) {
	var posted string
	app := newReviewApp(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(b, &body)
		posted = body.Body
		io.WriteString(w, `{"id":1,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-1"}`)
	})
	app.SetFooter("0.51.26", "https://quack.example.com")
	if _, err := app.submitReview(context.Background(), submitReviewArgs{
		Owner: "acme", Repo: "widgets", PullNumber: 7, Body: "LGTM", Event: "APPROVE", ChatID: "chat1",
	}); err != nil {
		t.Fatalf("submitReview: %v", err)
	}
	if !strings.Contains(posted, deliveryMarker("review")) {
		t.Errorf("posted review lost its delivery marker: %q", posted)
	}
	want := `<sub>quack 0.51.26 · <a href="https://quack.example.com/chat/chat1">run</a></sub>`
	if !strings.HasSuffix(strings.TrimRight(posted, "\n"), want) {
		t.Errorf("posted review does not end with the footer: %q", posted)
	}
}

func TestCommandsBlockTextOmitsDisabledTriggers(t *testing.T) {
	labels := Labels{Review: "quack-auto-review", Merge: "quack:merge", Fix: "quack:fix", Plan: "quack:plan", Implement: "quack:implement"}
	tests := []struct {
		name     string
		triggers map[string]bool
		wantHas  []string
		wantNot  []string
	}{
		{
			name:     "only mention",
			triggers: map[string]bool{"mention": true},
			wantHas:  []string{"/quack <request>"},
			wantNot:  []string{"/review", "/explain", "quack:merge", "quack:fix", "quack:plan", "quack:implement"},
		},
		{
			name:     "label and merge",
			triggers: map[string]bool{"label": true, "merge": true},
			wantHas:  []string{"/review", "quack-auto-review", "quack:merge"},
			wantNot:  []string{"/quack <request>", "quack:fix", "quack:plan"},
		},
		{
			name:     "everything",
			triggers: map[string]bool{"mention": true, "label": true, "merge": true, "ci_fix": true, "issue_plan": true, "issue_implement": true, "explain": true},
			wantHas:  []string{"/quack <request>", "/review", "/explain", "quack-auto-review", "quack:merge", "quack:fix", "quack:plan", "quack:implement"},
		},
		{
			name:     "nothing enabled",
			triggers: map[string]bool{"pr_opened": true},
			wantHas:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandsBlockText("/quack", labels, tt.triggers)
			if len(tt.wantHas) == 0 {
				if got != "" {
					t.Errorf("commandsBlockText() = %q, want empty", got)
				}
				return
			}
			for _, s := range tt.wantHas {
				if !strings.Contains(got, s) {
					t.Errorf("commandsBlockText() missing %q: %q", s, got)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(got, s) {
					t.Errorf("commandsBlockText() unexpectedly contains %q: %q", s, got)
				}
			}
		})
	}
}

func TestWithReviewFooterRendersBlockWithNoVersionOrURL(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.SetReviewCommands("/quack", Labels{Review: "quack-auto-review"}, map[string]bool{"mention": true, "label": true})
	got := app.withReviewFooter("LGTM", "chat1")
	if !strings.Contains(got, "<details>") || !strings.Contains(got, "/quack <request>") {
		t.Errorf("withReviewFooter with no version/URL lost the commands block: %q", got)
	}
	if strings.Contains(got, "<sub>quack") {
		t.Errorf("withReviewFooter added a plain footer despite no version/URL: %q", got)
	}
}

func TestWithReviewFooterIdempotentAndSingleBlock(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.SetReviewCommands("/quack", Labels{Review: "quack-auto-review", Merge: "quack:merge"}, map[string]bool{"mention": true, "label": true, "merge": true})
	app.SetFooter("0.51.26", "https://quack.example.com")

	once := app.withReviewFooter("LGTM", "chat1")
	if n := strings.Count(once, "<details>"); n != 1 {
		t.Fatalf("commands block appears %d times, want 1: %q", n, once)
	}
	twice := app.withReviewFooter(once, "chat1")
	if twice != once {
		t.Errorf("re-rendering an already-footed review body changed it: got %q, want %q", twice, once)
	}
	if n := strings.Count(twice, "<details>"); n != 1 {
		t.Errorf("commands block appears %d times after re-render, want 1: %q", n, twice)
	}
	if !strings.HasSuffix(twice, "</sub>") {
		t.Errorf("plain footer not last: %q", twice)
	}
}

// A later check-outcome line slots above the whole trailing region, not between the commands block and <sub>.
func TestAppendLineAboveCommandsBlockAndFooter(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.SetReviewCommands("/quack", Labels{Review: "quack-auto-review"}, map[string]bool{"label": true})
	app.SetFooter("0.51.26", "https://quack.example.com")
	body := app.withReviewFooter("LGTM", "chat1")

	got, changed := appendLine(body, "Merged as abc1234.")
	if !changed {
		t.Fatal("expected the outcome line to be added")
	}
	lineIdx := strings.Index(got, "Merged as abc1234.")
	blockIdx := strings.Index(got, "<details>")
	footerIdx := strings.Index(got, "<sub>quack")
	if !(lineIdx < blockIdx && blockIdx < footerIdx) {
		t.Errorf("outcome line not slotted above the block and footer: %q", got)
	}
}

func mustTestKeyPEM(t *testing.T) string {
	t.Helper()
	pem, _ := testKeyPEM(t)
	return pem
}

// An agent's own trailing <details> section is review content, not the footer.
func TestAppendLineIgnoresAgentDetailsBlock(t *testing.T) {
	agent := "Summary.\n\n<details>\n<summary>Full findings</summary>\n\nlong list\n\n</details>"
	got, added := appendLine(agent, "Merged.")
	if !added || !strings.HasSuffix(got, "</details>\n\nMerged.") {
		t.Fatalf("line must land after the agent's own section, got:\n%s", got)
	}

	a := &App{}
	a.SetFooter("1.2.3", "https://quack.example")
	a.SetReviewCommands("/quack", Labels{Review: "quack:review"}, map[string]bool{"mention": true, "label": true})
	body := a.withReviewFooter(agent, "chat-1")
	got, _ = appendLine(body, "Merged.")
	iAgent, iLine, iCmd := strings.Index(got, "Full findings"), strings.Index(got, "Merged."), strings.Index(got, commandsSummary)
	if !(iAgent < iLine && iLine < iCmd) {
		t.Fatalf("want agent section, then the line, then the commands block; got:\n%s", got)
	}
}
