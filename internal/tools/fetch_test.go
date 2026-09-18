package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// fakeFetchResult is one URL's canned fetcher outcome.
type fakeFetchResult struct {
	body string
	err  error
}

// fakeFetcher: fetcher double keyed by the validated target URL, so
// fetchBatch/fetchOne can be exercised without real HTTP.
type fakeFetcher map[string]fakeFetchResult

func (f fakeFetcher) fetch(_ adkagent.Context, _ Deps, _ *url.URL, target string) (string, error) {
	r, ok := f[target]
	if !ok {
		return "", fmt.Errorf("fakeFetcher: no stub for %s", target)
	}
	return r.body, r.err
}

// testRecordStore: a chat-scoped recordstore.Client over an in-memory
// artifact.Service, for tests that store or read web_page artifacts.
func testRecordStore() *recordstore.Client {
	return recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
}

func TestFetchBatch_InlineUnderThreshold_ArtifactAtThreshold(t *testing.T) {
	small := "short page body, well under the threshold"
	large := strings.Repeat("word ", (fetchArtifactThreshold/5)+100) // pushes len(full) over fetchArtifactThreshold
	f := fakeFetcher{
		"https://ex.com/small": {body: small},
		"https://ex.com/large": {body: large},
	}
	rc := testRecordStore()
	d := Deps{RecordStore: rc, NodeID: "n1", Coords: &RoundCoords{Round: 1}}

	results := fetchBatch(newFakeCtx(), d, f, []string{"https://ex.com/small", "https://ex.com/large"}, "", 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	small_, large_ := results[0], results[1]

	if small_.Error != "" {
		t.Errorf("small: unexpected error %q", small_.Error)
	}
	if strings.Contains(small_.Text, "artifact:") {
		t.Errorf("small page under threshold stored as an artifact: %q", small_.Text)
	}
	if !strings.Contains(small_.Text, "short page body") {
		t.Errorf("small: text = %q, want the page body inline", small_.Text)
	}

	if large_.Error != "" {
		t.Errorf("large: unexpected error %q", large_.Error)
	}
	if !strings.Contains(large_.Text, "artifact:") || !strings.Contains(large_.Text, "lines:") {
		t.Errorf("large page at/above threshold: text = %q, want a stored-artifact header", large_.Text)
	}

	items, err := rc.List(context.Background(), kindWebPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("web_page artifacts = %d, want exactly 1 (only the large page stored)", len(items))
	}
	raw, _, ok, err := rc.Latest(context.Background(), items[0].ID)
	if err != nil || !ok {
		t.Fatalf("Latest(%s): ok=%v err=%v", items[0].ID, ok, err)
	}
	if !strings.Contains(string(raw), large) {
		t.Errorf("stored artifact content missing the full fetched page")
	}
	if !strings.Contains(string(raw), "url: https://ex.com/large") {
		t.Errorf("stored artifact content missing its url metadata line:\n%s", string(raw)[:200])
	}
}

func TestFetchBatch_PerURLFailureDoesNotFailBatch(t *testing.T) {
	f := fakeFetcher{
		"https://ex.com/ok": {body: "all good"},
	}
	d := Deps{}
	results := fetchBatch(newFakeCtx(), d, f, []string{"https://ex.com/ok", "https://ex.com/missing"}, "", 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Error != "" || !strings.Contains(results[0].Text, "all good") {
		t.Errorf("ok entry = %+v, want text set and no error", results[0])
	}
	if results[1].Error == "" {
		t.Errorf("missing entry = %+v, want a per-URL error", results[1])
	}
}

func TestShapeOrStore_PatternShortcutStillStores(t *testing.T) {
	// One short needle line among many filler lines, so the matched line itself
	// (not just the whole page) stays well under capFetchReturn's byte cap.
	large := strings.Repeat("filler line\n", (fetchArtifactThreshold/12)+50) + "needle-marker here\n"
	rc := testRecordStore()
	d := Deps{RecordStore: rc, NodeID: "n1"}

	got := shapeOrStore(newFakeCtx(), d, "https://ex.com/large", large, "needle-marker", 0)
	if strings.Contains(got, "artifact:") {
		t.Errorf("pattern shortcut should grep the page directly, not return the stored header: %q", got)
	}
	if !strings.Contains(got, "needle-marker") {
		t.Errorf("pattern shortcut result = %q, want the matching line", got)
	}
	items, err := rc.List(context.Background(), kindWebPage)
	if err != nil || len(items) != 1 {
		t.Fatalf("web_page artifacts = %d (err=%v), want the page stored despite the pattern shortcut", len(items), err)
	}
}

func TestShapeOrStore_NoRecordStoreFallsBackInline(t *testing.T) {
	large := strings.Repeat("word ", (fetchArtifactThreshold/5)+100)
	got := shapeOrStore(newFakeCtx(), Deps{}, "https://ex.com/large", large, "", 0)
	if strings.Contains(got, "artifact:") {
		t.Errorf("no RecordStore configured: should degrade to inline text, got %q", got[:60])
	}
}

func TestWindowLines(t *testing.T) {
	lines := []string{"line-a", "line-b", "line-c", "line-d", "line-e"}
	// body is everything before the "\n\n[" navigation footer.
	body := func(s string) string {
		if i := strings.Index(s, "\n\n["); i >= 0 {
			return s[:i]
		}
		return s
	}
	cases := []struct {
		name        string
		start, want int
		wantBody    string // "" only for the past-the-end case, checked separately
	}{
		{"default window head", 1, 0, "line-a\nline-b\nline-c\nline-d\nline-e"},
		{"custom window size", 2, 2, "line-b\nline-c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := body(windowLines(lines, tc.start, tc.want, len(lines)))
			if got != tc.wantBody {
				t.Errorf("windowLines() body = %q, want %q", got, tc.wantBody)
			}
		})
	}

	if got := windowLines(lines, 100, 0, len(lines)); !strings.Contains(got, "past the end") {
		t.Errorf("start past total = %q, want a past-the-end message", got)
	}
	if got := windowLines(lines, 1, maxWindowLines+1000, len(lines)); body(got) != strings.Join(lines, "\n") {
		t.Errorf("want beyond maxWindowLines still returns the whole (short) page, got %q", got)
	}
}

func TestWebPageIdentity(t *testing.T) {
	id1, err := webPageIdentity([]byte("content v1"), "https://ex.com/a")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := webPageIdentity([]byte("content v2, totally different"), "https://ex.com/a")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("identity depends on content (%q vs %q); refetching the same URL must keep the same artifact id", id1, id2)
	}
	if _, err := webPageIdentity([]byte("x"), ""); err == nil {
		t.Error("empty hint (no url) should error")
	}
}

func TestPageTitle(t *testing.T) {
	cases := map[string]string{
		"# Heading\nbody text": "Heading",
		"\n\nplain first line": "plain first line",
		"":                     "",
		"   \n  \n":            "",
	}
	for in, want := range cases {
		if got := pageTitle(in); got != want {
			t.Errorf("pageTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
