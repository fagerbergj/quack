package tools

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestRegistryBuild(t *testing.T) {
	if !Known("web_search") || !Known("web_fetch") || !Known("summarize") {
		t.Fatal("expected web_search/fetch/summarize to be known")
	}
	if Known("nope") {
		t.Fatal("unknown tool reported as known")
	}
	if _, err := Build([]string{"web_fetch"}, Deps{Crawl4AI: "http://x"}); err != nil {
		t.Fatalf("Build(fetch) error: %v", err)
	}
	if _, err := Build([]string{"bogus"}, Deps{}); err == nil {
		t.Fatal("Build(bogus) should error")
	}
	if _, err := Build([]string{"web_search"}, Deps{}); err == nil {
		t.Fatal("web_search without SearXNG should error")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/md") {
			t.Errorf("unexpected crawl4ai call: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["url"] == "" {
			t.Error("md request missing url")
		}
		if body["f"] != "fit" {
			t.Errorf("md request filter = %v, want fit", body["f"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"markdown":"# Rendered\n\n[a link](https://example.com/x)","success":true}`)
	}))
	defer srv.Close()

	text, err := crawl4aiMarkdown(context.Background(), srv.Client(), srv.URL, "http://example.com/spa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[a link](https://example.com/x)") {
		t.Errorf("crawl4aiMarkdown = %q, want the markdown with its link", text)
	}
}

func TestCrawl4AIMarkdownFitFallsBackToRaw(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		filter, _ := body["f"].(string)
		calls = append(calls, filter)
		w.Header().Set("Content-Type", "application/json")
		if filter == "fit" {
			io.WriteString(w, `{"markdown":"   ","success":true}`) // fit pruned everything
			return
		}
		io.WriteString(w, `{"markdown":"raw body text","success":true}`)
	}))
	defer srv.Close()

	text, err := crawl4aiMarkdown(context.Background(), srv.Client(), srv.URL, "http://example.com/spa")
	if err != nil {
		t.Fatal(err)
	}
	if text != "raw body text" {
		t.Errorf("crawl4aiMarkdown = %q, want the raw fallback", text)
	}
	if len(calls) != 2 || calls[0] != "fit" || calls[1] != "raw" {
		t.Errorf("filter calls = %v, want [fit raw]", calls)
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

	got, err := searchWeb(context.Background(), srv.Client(), srv.URL, "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Snippet != "snippet a" || got[1].URL != "http://b" {
		t.Errorf("searchWeb = %+v, want both parsed results", got)
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
