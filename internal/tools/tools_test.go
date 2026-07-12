package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// emptyOKRoundTripper answers every request with an empty 200, standing in for a
// reachable page that yields no readable text without a browser render.
type emptyOKRoundTripper struct{}

func (emptyOKRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

func TestRegistryBuild(t *testing.T) {
	if _, err := Build([]string{"web_fetch"}, Deps{Fetch: Backend{URL: "http://x"}}); err != nil {
		t.Fatalf("Build(fetch) error: %v", err)
	}
	if _, err := Build([]string{"bogus"}, Deps{}); err == nil {
		t.Fatal("Build(bogus) should error")
	}
	if _, err := Build([]string{"web_search"}, Deps{}); err == nil {
		t.Fatal("web_search without a backend URL should error")
	}
	if _, err := Build([]string{"summarize"}, Deps{}); err == nil {
		t.Fatal("summarize without a model should error")
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	in := `<html><head><title>t</title><style>.x{}</style></head>
	<body><h1>Hello</h1><script>var x=1;</script>
	<p>World of <a href="https://example.com/page">text</a></p></body></html>`
	got, err := htmlToMarkdown(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Hello") {
		t.Errorf("htmlToMarkdown = %q, want an ATX heading", got)
	}
	// The whole point: links survive so the researcher can cite them.
	if !strings.Contains(got, "[text](https://example.com/page)") {
		t.Errorf("htmlToMarkdown = %q, want the link preserved", got)
	}
	if strings.Contains(got, "var x") || strings.Contains(got, ".x{}") {
		t.Errorf("htmlToMarkdown = %q, should drop script/style content", got)
	}
}

func TestHTMLToMarkdownDropsChrome(t *testing.T) {
	in := `<html><body><nav>Home Sign in Open in app</nav><header>Site Logo</header>
	<article><p>The real article body.</p></article>
	<footer>Help Status About Careers Privacy</footer></body></html>`
	got, err := htmlToMarkdown(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "The real article body") {
		t.Errorf("htmlToMarkdown = %q, want the article body", got)
	}
	for _, chrome := range []string{"Sign in", "Open in app", "Site Logo", "Careers"} {
		if strings.Contains(got, chrome) {
			t.Errorf("htmlToMarkdown = %q, should drop nav/header/footer chrome %q", got, chrome)
		}
	}
}

func TestLooksLikeBotWall(t *testing.T) {
	walls := []string{
		"www.peerspace.com Performing security verification This website uses a security service to protect against malicious bots. Ray ID: a06c41ff",
		"Reddit - Please wait for verification",
		"You've been blocked by network security.",
	}
	for _, w := range walls {
		if !looksLikeBotWall(w) {
			t.Errorf("looksLikeBotWall(%q) = false, want true", w)
		}
	}
	// Real content must not trip it, even when long text mentions a marker word.
	real := "Vancouver offers great summer activities. " + strings.Repeat("Kayaking, biking, and beaches. ", 200)
	if looksLikeBotWall(real) {
		t.Error("looksLikeBotWall(real content) = true, want false")
	}
	if looksLikeBotWall("") {
		t.Error("looksLikeBotWall(empty) = true, want false")
	}
}

func TestFetchReadableDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body><p>"+strings.Repeat("alpha beta ", 50)+"</p></body></html>")
	}))
	defer srv.Close()

	// httptest is loopback; the production guarded client would block it, so we
	// use a plain client here. The guard itself is covered by safefetch_test.go.
	text, err := fetchReadable(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "alpha beta") {
		t.Errorf("fetchReadable = %q, want page text", text)
	}
}

func TestCrawl4AIMarkdown(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/crawl") {
			t.Errorf("unexpected crawl4ai call: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"results":[{"success":true,"markdown":{`+
			`"fit_markdown":"# Rendered\n\n[a link](https://example.com/x)","raw_markdown":"raw"}}]}`)
	}))
	defer srv.Close()

	text, err := crawl4aiMarkdown(context.Background(), srv.Client(), srv.URL, "http://example.com/spa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[a link](https://example.com/x)") {
		t.Errorf("crawl4aiMarkdown = %q, want the fit markdown with its link", text)
	}
	// The settle-wait is the whole point of the fix: the request must carry the
	// crawler_config that tells crawl4ai to wait for the page to stop navigating.
	params, _ := gotBody["crawler_config"].(map[string]any)
	if params != nil {
		params, _ = params["params"].(map[string]any)
	}
	if params == nil {
		t.Fatalf("crawl request missing crawler_config.params; body = %v", gotBody)
	}
	if params["wait_until"] != crawl4aiWaitUntil {
		t.Errorf("wait_until = %v, want %q", params["wait_until"], crawl4aiWaitUntil)
	}
	if params["delay_before_return_html"] == nil {
		t.Error("crawl request missing delay_before_return_html settle delay")
	}
	if params["page_timeout"] == nil {
		t.Error("crawl request missing page_timeout bound")
	}
}

func TestCrawl4AIMarkdownFitFallsBackToRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// fit pruned everything; raw still has the body.
		io.WriteString(w, `{"success":true,"results":[{"success":true,"markdown":{`+
			`"fit_markdown":"   ","raw_markdown":"raw body text"}}]}`)
	}))
	defer srv.Close()

	text, err := crawl4aiMarkdown(context.Background(), srv.Client(), srv.URL, "http://example.com/spa")
	if err != nil {
		t.Fatal(err)
	}
	if text != "raw body text" {
		t.Errorf("crawl4aiMarkdown = %q, want the raw fallback", text)
	}
}

// TestFetchViaRenderFailureDegrades: a crawl4ai 500 on a page the direct GET could
// reach (but which had no readable text without a browser) must not fail the whole
// fetch — it degrades to an honest, clearly-marked render-unavailable result, with
// a WARN logged, so one flaky dynamic page doesn't sink a research node.
func TestFetchViaRenderFailureDegrades(t *testing.T) {
	render := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Page.content: page is navigating and changing the content", http.StatusInternalServerError)
	}))
	defer render.Close()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	d := Deps{Guarded: &http.Client{Transport: emptyOKRoundTripper{}}}
	renderer := &crawl4aiRenderer{client: render.Client(), base: render.URL}
	// A non-blocked literal IP so validateResolvedHost passes and the render backend
	// is actually consulted (and 500s).
	u, _ := url.Parse("http://93.184.216.34/")

	got, err := fetchVia(context.Background(), d, renderer, u, u.String())
	if err != nil {
		t.Fatalf("render 500 hard-failed the fetch: %v", err)
	}
	if !strings.Contains(got, "render backend could not retrieve") {
		t.Errorf("degraded result = %q, want a clearly-marked render-unavailable message", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "render backend failed") {
		t.Errorf("expected a WARN log for the render failure; logs = %q", logs.String())
	}
}

// TestFetchViaTargetFailureStillErrors: a genuine target failure (direct GET errors)
// is never masked as success, even when the render backend also fails.
func TestFetchViaTargetFailureStillErrors(t *testing.T) {
	render := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer render.Close()

	// Direct GET returns 404 → derr != nil, so this is a real fetch failure.
	d := Deps{Guarded: &http.Client{Transport: statusRoundTripper{http.StatusNotFound}}}
	renderer := &crawl4aiRenderer{client: render.Client(), base: render.URL}
	u, _ := url.Parse("http://93.184.216.34/")

	if _, err := fetchVia(context.Background(), d, renderer, u, u.String()); err == nil {
		t.Error("a 404 target with a failed render must still error, not degrade to success")
	}
}

// statusRoundTripper answers every request with the given status and an empty body.
type statusRoundTripper struct{ code int }

func (s statusRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.code,
		Status:     http.StatusText(s.code),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

func TestShapeFetchResultHeadAndOffset(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	full := b.String()

	head := shapeFetchResult(full, "", 0)
	if !strings.Contains(head, "line 1\n") || !strings.Contains(head, fmt.Sprintf("line %d", fetchHeadLines)) {
		t.Errorf("head missing expected lines")
	}
	if strings.Contains(head, fmt.Sprintf("\nline %d\n", fetchHeadLines+1)) {
		t.Errorf("head leaked a line past the window")
	}
	if !strings.Contains(head, "offset=") {
		t.Errorf("head footer should hint offset=: %q", head)
	}

	win := shapeFetchResult(full, "", 150)
	if !strings.Contains(win, "line 150") || strings.Contains(win, "\nline 149\n") {
		t.Errorf("offset window should start at line 150")
	}
}

func TestShapeFetchResultGrep(t *testing.T) {
	full := "Apple pie\nBanana split\napple turnover\nCherry\n"

	out := shapeFetchResult(full, "apple", 0)
	if !strings.Contains(out, "1: Apple pie") || !strings.Contains(out, "3: apple turnover") {
		t.Errorf("grep should match case-insensitively with line numbers: %q", out)
	}
	if strings.Contains(out, "Banana") {
		t.Errorf("grep returned a non-matching line: %q", out)
	}

	if none := shapeFetchResult(full, "zzz", 0); !strings.Contains(none, "no lines match") {
		t.Errorf("expected a no-match message: %q", none)
	}

	// An invalid regex ("(") falls back to a literal substring search.
	lit := shapeFetchResult("a (b) c\nx y z\n", "(", 0)
	if !strings.Contains(lit, "1: a (b) c") || strings.Contains(lit, "x y z") {
		t.Errorf("invalid regex should fall back to literal substring: %q", lit)
	}
}

func TestSearchWebParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("q") != "golang" {
			t.Errorf("expected q=golang, got %q", r.URL.Query().Get("q"))
		}
		io.WriteString(w, `{"results":[
			{"title":"A","url":"http://a","content":"snippet a"},
			{"title":"B","url":"http://b","content":"snippet b"}
		]}`)
	}))
	defer srv.Close()

	got, _, err := searchWeb(context.Background(), srv.Client(), srv.URL, "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Snippet != "snippet a" || got[1].URL != "http://b" {
		t.Errorf("searchWeb = %+v, want both parsed results", got)
	}
}

// SearXNG answers HTTP 200 even when every upstream engine 429s, listing the
// failures only in unresponsive_engines. searchWeb must turn an all-failed,
// zero-result search into an error so the agent sees the rate limit instead of a
// silently empty list.
func TestSearchWebSurfacesRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[],"unresponsive_engines":[
			["brave","Too many requests"],
			["google","timeout"]
		]}`)
	}))
	defer srv.Close()

	_, _, err := searchWeb(context.Background(), srv.Client(), srv.URL, "golang")
	if err == nil {
		t.Fatal("expected an error when all backends are rate-limited, got nil")
	}
	if !strings.Contains(err.Error(), "brave") || !strings.Contains(err.Error(), "Too many requests") {
		t.Errorf("error should name the failed engine and reason, got: %v", err)
	}
}

// When some backends fail but others return hits, searchWeb returns the hits
// plus a non-fatal note so the agent knows coverage was reduced.
func TestSearchWebPartialResultsNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[
			{"title":"A","url":"http://a","content":"snippet a"}
		],"unresponsive_engines":[["brave","Too many requests"]]}`)
	}))
	defer srv.Close()

	got, note, err := searchWeb(context.Background(), srv.Client(), srv.URL, "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("want the one available result, got %+v", got)
	}
	if !strings.Contains(note, "brave") {
		t.Errorf("note should name the failed backend, got: %q", note)
	}
}

// stubModel returns canned text for summarizeText, ignoring the request.
type stubModel struct{ out string }

func (stubModel) Name() string { return "stub" }
func (m stubModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "thinking", Thought: true}, {Text: m.out}}},
			TurnComplete: true,
		}, nil)
	}
}

func TestSummarizeText(t *testing.T) {
	got, err := summarizeText(context.Background(), stubModel{out: "a short summary"}, "a very long text", "the key point")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a short summary" {
		t.Errorf("summarizeText = %q, want the model's non-thought text", got)
	}
	if _, err := summarizeText(context.Background(), stubModel{out: "x"}, "   ", ""); err == nil {
		t.Error("summarizeText(empty) should error")
	}
}

func TestNewCommitMemoryTool(t *testing.T) {
	tl, err := NewCommitMemoryTool(nil, "u1") // construction only; handler not invoked
	if err != nil {
		t.Fatalf("NewCommitMemoryTool: %v", err)
	}
	if tl.Name() != "commit_memory" {
		t.Fatalf("name = %q, want commit_memory", tl.Name())
	}
}
