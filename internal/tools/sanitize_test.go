package tools

import "strings"

import "testing"

func TestStripInlineMediaDropsDataURI(t *testing.T) {
	// A base64 inline image (the "garbage in context" case) is valid UTF-8, so the
	// binary check misses it — stripInlineMedia must remove it.
	blob := strings.Repeat("AAAA", 5000) // ~20KB of base64
	in := "Real article text.\n![logo](data:image/png;base64," + blob + ")\nMore text."
	out := stripInlineMedia(in)
	if strings.Contains(out, blob) {
		t.Fatalf("data URI base64 survived: %d bytes out", len(out))
	}
	if !strings.Contains(out, "Real article text.") || !strings.Contains(out, "More text.") {
		t.Errorf("stripped real text too: %q", out)
	}
}

func TestCollapseLongTokensKeepsProseAndURLs(t *testing.T) {
	prose := "The quick brown fox. See https://example.com/some/long/path?x=1&y=2 for details."
	if got := collapseLongTokens(prose); got != prose {
		t.Errorf("prose/URL altered:\n got %q\nwant %q", got, prose)
	}
	// A 5000-char unbroken run (no data: prefix) is still collapsed.
	garbage := "before " + strings.Repeat("x", 5000) + " after"
	out := collapseLongTokens(garbage)
	if strings.Contains(out, strings.Repeat("x", 5000)) {
		t.Errorf("long token survived")
	}
	if !strings.Contains(out, "before ") || !strings.Contains(out, " after") {
		t.Errorf("dropped surrounding text: %q", out)
	}
}

func TestIsUnreadableContentType(t *testing.T) {
	unreadable := []string{"image/jpeg", "video/mp4", "audio/mpeg", "font/woff2", "application/octet-stream", "application/zip"}
	for _, ct := range unreadable {
		if !isUnreadableContentType(ct) {
			t.Errorf("%q should be unreadable", ct)
		}
	}
	readable := []string{"text/html; charset=utf-8", "text/plain", "application/json", "text/markdown"}
	for _, ct := range readable {
		if isUnreadableContentType(ct) {
			t.Errorf("%q should be readable", ct)
		}
	}
}
