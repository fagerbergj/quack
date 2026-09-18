package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
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

// fakeFetcher: fetcher double keyed by the validated target URL, counting
// calls per URL so a test can prove a dedup or cache-hit path skipped a refetch.
type fakeFetcher struct {
	mu    sync.Mutex
	stubs map[string]fakeFetchResult
	calls map[string]int
}

func newFakeFetcher(stubs map[string]fakeFetchResult) *fakeFetcher {
	return &fakeFetcher{stubs: stubs, calls: map[string]int{}}
}

func (f *fakeFetcher) fetch(_ adkagent.Context, _ Deps, _ *url.URL, target string) (string, error) {
	f.mu.Lock()
	f.calls[target]++
	f.mu.Unlock()
	r, ok := f.stubs[target]
	if !ok {
		return "", fmt.Errorf("fakeFetcher: no stub for %s", target)
	}
	return r.body, r.err
}

func (f *fakeFetcher) callCount(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[target]
}

// testRecordStore: a chat-scoped recordstore.Client over an in-memory
// artifact.Service, for tests that store or read web_page artifacts.
func testRecordStore() *recordstore.Client {
	return recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
}

func TestFetchBatch_InlineUnderThreshold_ArtifactAtThreshold(t *testing.T) {
	small := "short page body, well under the threshold"
	large := strings.Repeat("word ", (fetchArtifactThreshold/5)+100) // pushes len(full) over fetchArtifactThreshold
	f := newFakeFetcher(map[string]fakeFetchResult{
		"https://ex.com/small": {body: small},
		"https://ex.com/large": {body: large},
	})
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
	if small_.Text != small {
		t.Errorf("small: text = %q, want the exact page body inline (%q)", small_.Text, small)
	}

	if large_.Error != "" {
		t.Errorf("large: unexpected error %q", large_.Error)
	}
	if !strings.Contains(large_.Text, "artifact:") || !strings.Contains(large_.Text, "lines:") || !strings.Contains(large_.Text, "bytes:") {
		t.Errorf("large page at/above threshold: text = %q, want a stored-artifact header with lines/bytes", large_.Text)
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
	if string(raw) != large {
		t.Errorf("stored artifact content should be the fetched page verbatim, no metadata prefix")
	}
}

// TestFetchBatch_SubThresholdManyLinesReturnsWholePage: a page under the byte
// threshold but over fetchHeadLines must still come back in full, not a head.
func TestFetchBatch_SubThresholdManyLinesReturnsWholePage(t *testing.T) {
	var lines []string
	for i := 0; i < fetchHeadLines*3; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	full := strings.Join(lines, "\n")
	if len(full) >= fetchArtifactThreshold {
		t.Fatalf("test setup: page is %d bytes, want under fetchArtifactThreshold (%d)", len(full), fetchArtifactThreshold)
	}
	f := newFakeFetcher(map[string]fakeFetchResult{"https://ex.com/many-lines": {body: full}})
	results := fetchBatch(newFakeCtx(), Deps{}, f, []string{"https://ex.com/many-lines"}, "", 0)
	if results[0].Text != full {
		t.Fatalf("sub-threshold page with %d lines was not returned whole: got %d bytes, want %d", len(lines), len(results[0].Text), len(full))
	}
}

func TestFetchBatch_PerURLFailureDoesNotFailBatch(t *testing.T) {
	f := newFakeFetcher(map[string]fakeFetchResult{
		"https://ex.com/ok": {body: "all good"},
	})
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

// TestFetchBatch_DedupesRepeatedURL pins nit 7: a URL requested twice in one
// batch is fetched once, and both positions get a result.
func TestFetchBatch_DedupesRepeatedURL(t *testing.T) {
	f := newFakeFetcher(map[string]fakeFetchResult{"https://ex.com/x": {body: "hello"}})
	results := fetchBatch(newFakeCtx(), Deps{}, f, []string{"https://ex.com/x", "https://ex.com/x"}, "", 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one per requested position)", len(results))
	}
	if results[0].Text != "hello" || results[1].Text != "hello" {
		t.Fatalf("results = %+v, want both positions to carry the fetched text", results)
	}
	if n := f.callCount("https://ex.com/x"); n != 1 {
		t.Errorf("fetcher called %d times for a repeated URL, want 1", n)
	}
}

// TestFetchBatch_InlineBudgetForcesStorage: past maxBatchInlineBytes
// cumulative, later pages store even though each is under fetchArtifactThreshold.
func TestFetchBatch_InlineBudgetForcesStorage(t *testing.T) {
	// Whitespace-separated, not one giant token, so collapseLongTokens leaves it alone.
	pageSize := fetchArtifactThreshold - 100
	body := strings.Repeat("word ", pageSize/5+1)[:pageSize]
	n := maxBatchInlineBytes/pageSize + 2 // guarantee the budget runs out before the last page
	urls := make([]string, n)
	stubs := map[string]fakeFetchResult{}
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("https://ex.com/p%d", i)
		urls[i] = u
		stubs[u] = fakeFetchResult{body: body}
	}
	f := newFakeFetcher(stubs)
	rc := testRecordStore()
	d := Deps{RecordStore: rc, NodeID: "n1"}

	results := fetchBatch(newFakeCtx(), d, f, urls, "", 0)
	var inlined, stored int
	for _, r := range results {
		if r.Error != "" {
			t.Fatalf("unexpected error: %+v", r)
		}
		if strings.Contains(r.Text, "artifact:") {
			stored++
		} else {
			inlined++
		}
	}
	if stored == 0 {
		t.Fatalf("no page stored under budget pressure: all %d pages inlined (%d bytes each) - batch budget (%d) not enforced", n, pageSize, maxBatchInlineBytes)
	}
	if inlined == 0 {
		t.Fatalf("every page stored - expected at least the first (budget starts full)")
	}
}

func TestShapeOrStore_PatternShortcutStillStores(t *testing.T) {
	// One short needle line among many filler lines, so the matched line itself
	// (not just the whole page) stays well under capFetchReturn's byte cap.
	large := strings.Repeat("filler line\n", (fetchArtifactThreshold/12)+50) + "needle-marker here\n"
	rc := testRecordStore()
	d := Deps{RecordStore: rc, NodeID: "n1"}

	got, inlined := shapeOrStore(newFakeCtx(), d, fetchedPage{url: "https://ex.com/large", full: large}, "needle-marker", 0, true)
	if inlined {
		t.Errorf("pattern shortcut result should never count against the inline budget")
	}
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

func TestShapeOrStore_NoRecordStoreFallsBackToHead(t *testing.T) {
	large := strings.Repeat("word ", (fetchArtifactThreshold/5)+100)
	got, inlined := shapeOrStore(newFakeCtx(), Deps{}, fetchedPage{url: "https://ex.com/large", full: large}, "", 0, true)
	if inlined {
		t.Errorf("no RecordStore, over threshold: must not count as inlined")
	}
	if strings.Contains(got, "artifact:") {
		t.Errorf("no RecordStore configured: should degrade to inline text, got %q", got[:60])
	}
}

// TestStoreWebPage_CacheHitReusesExistingID pins nit 6: a cache hit on an
// already-stored page reuses its id instead of writing a duplicate revision.
func TestStoreWebPage_CacheHitReusesExistingID(t *testing.T) {
	rc := testRecordStore()
	d := Deps{RecordStore: rc, NodeID: "n1"}
	full := strings.Repeat("y", fetchArtifactThreshold+500)
	ctx := newFakeCtx()

	first := storeWebPage(ctx, d, "https://ex.com/cached", full, false)
	second := storeWebPage(ctx, d, "https://ex.com/cached", full, true)

	firstID := headerField(t, first, "artifact")
	secondID := headerField(t, second, "artifact")
	if firstID != secondID {
		t.Fatalf("cache-hit store reused a different id: first=%s second=%s", firstID, secondID)
	}
	revs, err := rc.Versions(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 {
		t.Fatalf("revisions = %v, want exactly 1 (cache hit must not write a second one)", revs)
	}
}

// TestWebPageHeader_ByteSizeAndTruncation pins nit 9: the header names the
// byte size and flags a fetch that hit maxFetchBytes.
func TestWebPageHeader_ByteSizeAndTruncation(t *testing.T) {
	content := "line one\nline two"
	got := webPageHeader("T", "https://ex.com/x", "web_page:abc", content, true)
	if !strings.Contains(got, fmt.Sprintf("bytes: %d", len(content))) {
		t.Errorf("header = %q, want a bytes: %d field", got, len(content))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("header = %q, want a truncation note", got)
	}
	untruncated := webPageHeader("T", "https://ex.com/x", "web_page:abc", content, false)
	if strings.Contains(untruncated, "truncated") {
		t.Errorf("untruncated header = %q, should not mention truncation", untruncated)
	}
}

// TestWebFetchTool_TooManyURLsErrors pins blocker 2's list-length cap.
func TestWebFetchTool_TooManyURLsErrors(t *testing.T) {
	tl, err := newFetch(Deps{})
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("web_fetch tool is not runnable")
	}
	urls := make([]string, maxBatchURLs+1)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://ex.com/%d", i)
	}
	if _, err := rt.Run(newFakeCtx(), map[string]any{"urls": urls}); err == nil {
		t.Fatal("a batch over maxBatchURLs should error")
	}
}

// headerField extracts "key: value" from a webPageHeader-shaped string.
func headerField(t *testing.T, header, key string) string {
	t.Helper()
	for _, ln := range strings.Split(header, "\n") {
		if v, ok := strings.CutPrefix(ln, key+": "); ok {
			return v
		}
	}
	t.Fatalf("header %q has no %q field", header, key)
	return ""
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
