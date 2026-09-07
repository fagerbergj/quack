package vetting

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/genai"
)

func writeRenderCheckPNGs(t *testing.T, dir string, names ...string) {
	t.Helper()
	shotDir := filepath.Join(dir, renderCheckScreenshotDir)
	if err := os.MkdirAll(shotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(shotDir, n), []byte("fake-png-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSanitizeLikeRenderCheck(t *testing.T) {
	got := sanitizeLikeRenderCheck("components/Composer.stories.tsx")
	want := "components_Composer_stories_tsx"
	if got != want {
		t.Errorf("sanitizeLikeRenderCheck = %q, want %q", got, want)
	}
}

func TestScreenshotMatchesChange(t *testing.T) {
	name := "_components_Composer_stories_tsx__Default__desktop__light.png"
	if !screenshotMatchesChange(name, []string{"frontend/src/components/Composer.stories.tsx"}) {
		t.Error("expected match against a changed story file")
	}
	if screenshotMatchesChange(name, []string{"frontend/src/components/Other.tsx"}) {
		t.Error("expected no match against an unrelated file")
	}
	if screenshotMatchesChange("no-separator.png", []string{"x.tsx"}) {
		t.Error("a filename with no __ separator must never match")
	}
}

func TestSelectScreenshotsCapsAndPrioritizesChanged(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("_components_AWidget%d_stories_tsx__Default__desktop__light.png", i))
	}
	// Sorts LAST alphabetically ("z..." after "AWidget...") so it only makes
	// the capped selection because it's the changed story, not by luck of sort order.
	changedName := "_components_zChanged_stories_tsx__Default__desktop__light.png"
	names = append(names, changedName)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := selectScreenshots(dir, []string{"frontend/src/components/zChanged.stories.tsx"})
	if len(got) != maxJudgeScreenshots {
		t.Fatalf("got %d screenshots, want cap of %d", len(got), maxJudgeScreenshots)
	}
	found := false
	for _, g := range got {
		if filepath.Base(g) == changedName {
			found = true
		}
	}
	if !found {
		t.Error("the changed-story screenshot must be prioritized into the capped selection")
	}
}

func TestSelectScreenshotsUnderCapReturnsAll(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("shot%d.png", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := selectScreenshots(dir, nil)
	if len(got) != 3 {
		t.Errorf("got %d screenshots, want 3 (under cap, return them all)", len(got))
	}
}

func TestSelectScreenshotsMissingDirReturnsNil(t *testing.T) {
	if got := selectScreenshots(filepath.Join(t.TempDir(), "nope"), nil); got != nil {
		t.Errorf("got %v, want nil for a missing render-check dir (e.g. render-check never ran)", got)
	}
}

func TestRenderScreenshotEvidenceNonFrontendGateReturnsNothing(t *testing.T) {
	cfg := testChecksConfig(t, []string{"go build"}, "")
	got := renderScreenshotEvidence(context.Background(), cfg, "n1", true, workerActivity{})
	if got != nil {
		t.Errorf("got %d parts, want none for a gate whose checks don't include render-check", len(got))
	}
}

func TestRenderScreenshotEvidenceChecksNotRunReturnsNothing(t *testing.T) {
	cfg := testChecksConfig(t, []string{renderCheckCommand}, "")
	got := renderScreenshotEvidence(context.Background(), cfg, "n1", false, workerActivity{})
	if got != nil {
		t.Errorf("got %d parts, want none when checksRan is false (checks were skipped this round)", len(got))
	}
}

func TestRenderScreenshotEvidenceAttachesCappedImageParts(t *testing.T) {
	cfg := testChecksConfig(t, []string{renderCheckCommand}, "")
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok {
		t.Fatalf("checksDir: ok=%v err=%v", ok, err)
	}
	var names []string
	for i := 0; i < 8; i++ {
		names = append(names, fmt.Sprintf("__c_stories_tsx__S%d__desktop__light.png", i))
	}
	writeRenderCheckPNGs(t, dir, names...)

	got := renderScreenshotEvidence(context.Background(), cfg, "n1", true, workerActivity{})
	if len(got) != maxJudgeScreenshots {
		t.Fatalf("got %d parts, want cap of %d", len(got), maxJudgeScreenshots)
	}
	for _, p := range got {
		if p.InlineData == nil || p.InlineData.MIMEType != "image/png" {
			t.Errorf("part = %+v, want an image/png InlineData part", p)
		}
	}
}

func TestAttachScreenshotsNoopWhenEmpty(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}
	if got := attachScreenshots(q, nil); got != q {
		t.Error("attachScreenshots with no shots must return the same question pointer unchanged")
	}
}

// TestAttachScreenshotsNeverMutatesQuestion proves the judge-only image
// evidence never leaks into the worker's own question/revision content -
// attachScreenshots must build a new Content, not append into question.Parts
// in place (a shared backing array would poison later reads of question).
func TestAttachScreenshotsNeverMutatesQuestion(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}
	shot := &genai.Part{InlineData: &genai.Blob{Data: []byte{1}, MIMEType: "image/png"}}

	got := attachScreenshots(q, []*genai.Part{shot})

	if len(q.Parts) != 1 {
		t.Fatalf("original question mutated: got %d parts, want 1", len(q.Parts))
	}
	if len(got.Parts) != 2 {
		t.Fatalf("got %d parts on the judge content, want 2 (text + screenshot)", len(got.Parts))
	}
	if got.Parts[0].Text != "hi" || got.Parts[1] != shot {
		t.Errorf("got %+v, want [text, shot] in order", got.Parts)
	}
}
