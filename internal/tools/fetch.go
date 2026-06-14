package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

const (
	// minUsefulText: a direct GET returning less readable text than this is
	// treated as "probably JS-rendered" and retried via the browser backend.
	minUsefulText = 200
	// maxFetchBytes caps how much of a page we read and keep in the cache — the
	// "full" copy that grep/offset slice. Generous so the whole page is available
	// to drill into; bounds the read and per-entry memory. Applied to both the
	// direct-GET read and the cached result (so a crawl4ai-rendered page is capped).
	maxFetchBytes = 200_000
	// fetchHeadLines: lines web_fetch returns by default (and per offset window).
	// The full page stays cached; the agent narrows in with grep= or offset=.
	fetchHeadLines = 120
	// fetchGrepMaxLines caps how many matching lines a grep returns.
	fetchGrepMaxLines = 120
	// fetchReturnMaxBytes hard-bounds any single web_fetch return so a page with
	// very long lines can't flood the context window regardless of mode.
	fetchReturnMaxBytes = 24_000
	// browserUA presents as a real desktop Chrome. Many sites serve bot-blocking
	// interstitials (or nothing) to obvious crawler UAs, so we look like a browser.
	browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	// fetchAccept prefers clean text formats: a few sites serve Markdown directly,
	// and we convert HTML to Markdown ourselves anyway.
	fetchAccept = "text/markdown;q=1.0, text/html;q=0.9, text/plain;q=0.8, */*;q=0.1"
)

// errCloudflareChallenge marks a direct GET that hit a Cloudflare bot challenge
// (403 + cf-mitigated: challenge), so the final error can name the cause.
var errCloudflareChallenge = errors.New("web_fetch: cloudflare challenge (cf-mitigated)")

type fetchArgs struct {
	URL string `json:"url"`
	// Pattern, when set, returns only the lines of the page matching this
	// (case-insensitive) regular expression, each prefixed with its line number.
	// The full page is cached, so this searches the whole page, not just the head.
	Pattern string `json:"pattern,omitempty"`
	// Offset, when > 0, returns a window of the page starting at this 1-based line
	// number (instead of the head). Use it to read past the head or around a line
	// a grep surfaced.
	Offset int `json:"offset,omitempty"`
}

// newFetch builds the fetch tool: SSRF-guard the URL, GET it with the guarded
// client, and fall back to the crawl4ai render backend for pages a bare GET
// can't read (JS-heavy or thin).
func newFetch(d Deps) (tool.Tool, error) {
	return functiontool.New[fetchArgs, string](
		functiontool.Config{
			Name:        "web_fetch",
			Description: "Fetch a web page by URL and return its readable text. Falls back to a headless browser for JavaScript-rendered pages. Long pages return only a head by default; the FULL page is retained, so pass `pattern` (a regex) to return just the matching lines, or `offset` (a line number) to read a window further down. Re-call the same URL with pattern/offset to drill in without re-paying the fetch.",
		},
		func(tc agent.ToolContext, a fetchArgs) (string, error) {
			u, err := ValidateURL(strings.TrimSpace(a.URL))
			if err != nil {
				return "", err
			}
			target := u.String()

			// Ensure the FULL page is in the cache; grep/offset slice that copy. On a
			// miss (first fetch, or the entry expired/evicted) we fetch and cache it.
			var full string
			if d.Cache != nil {
				if cached, ok := d.Cache.Get(tc, target); ok {
					full = cached
				}
			}
			if full == "" {
				fetched, ferr := fetchBest(tc, d, u, target)
				if ferr != nil {
					return "", ferr
				}
				if fetched, ferr = sanitizeFetched(target, fetched); ferr != nil {
					return "", ferr
				}
				// Cap the cached "full" copy so a huge (e.g. crawl4ai-rendered) page
				// can't bloat memory; 200KB holds essentially any real article.
				if len(fetched) > maxFetchBytes {
					fetched = strings.ToValidUTF8(fetched[:maxFetchBytes], "") + "\n[content truncated at fetch limit]"
				}
				full = fetched
				if d.Cache != nil {
					d.Cache.Set(tc, target, full, tc.SessionID(), tc.AppName())
				}
			}

			return shapeFetchResult(full, a.Pattern, a.Offset), nil
		},
	)
}

// shapeFetchResult decides what slice of a cached page to return: grep matches,
// an offset window, or (by default) the head. The full page stays cached so the
// agent can drill in with successive grep/offset calls on the same URL.
func shapeFetchResult(full, pattern string, offset int) string {
	lines := strings.Split(full, "\n")
	total := len(lines)
	if strings.TrimSpace(pattern) != "" {
		return grepPage(lines, pattern)
	}
	start := offset
	if start < 1 {
		start = 1
	}
	return windowPage(lines, start, total)
}

// windowPage returns up to fetchHeadLines lines starting at the 1-based `start`,
// with a footer telling the agent the range, the total, and how to read more.
func windowPage(lines []string, start, total int) string {
	if start > total {
		return fmt.Sprintf("[offset %d is past the end of this page (%d lines). Use a smaller offset or grep to search.]", start, total)
	}
	end := start + fetchHeadLines - 1
	if end > total {
		end = total
	}
	body := strings.Join(lines[start-1:end], "\n")
	var footer string
	if end < total {
		footer = fmt.Sprintf("\n\n[lines %d–%d of %d. Pass pattern=\"regex\" to search this page, or offset=%d to read further.]", start, end, total, end+1)
	} else {
		footer = fmt.Sprintf("\n\n[lines %d–%d of %d (end of page).]", start, end, total)
	}
	return capFetchReturn(body) + footer
}

// grepPage returns the page lines matching pattern (case-insensitive regex, with
// a literal-substring fallback for an invalid regex), each prefixed with its
// 1-based line number so the agent can follow up with offset=.
func grepPage(lines []string, pattern string) string {
	re, err := regexp.Compile("(?i)" + pattern)
	matchLine := func(s string) bool { return re.MatchString(s) }
	if err != nil {
		needle := strings.ToLower(strings.TrimSpace(pattern))
		matchLine = func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }
	}
	var matches []string
	capped := false
	for i, ln := range lines {
		if !matchLine(ln) {
			continue
		}
		if len(matches) >= fetchGrepMaxLines {
			capped = true
			break
		}
		matches = append(matches, fmt.Sprintf("%d: %s", i+1, strings.TrimSpace(ln)))
	}
	if len(matches) == 0 {
		return fmt.Sprintf("[no lines match %q in this page (%d lines). Try a broader pattern or offset=N to browse.]", pattern, len(lines))
	}
	footer := fmt.Sprintf("\n\n[%d matching line(s). Use offset=N to read the lines around a match.]", len(matches))
	if capped {
		footer = fmt.Sprintf("\n\n[first %d matches shown (more exist) — narrow the pattern, or offset=N to read around one.]", fetchGrepMaxLines)
	}
	return capFetchReturn(strings.Join(matches, "\n")) + footer
}

// capFetchReturn hard-bounds a return body so long lines can't flood context.
func capFetchReturn(s string) string {
	if len(s) <= fetchReturnMaxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:fetchReturnMaxBytes], "") + "\n[…truncated; narrow your grep or use offset=N]"
}

// fetchBest tries to get the best readable text for target. It first tries a
// direct GET; if that is thin, failed, or bot-walled, it falls back to crawl4ai.
func fetchBest(tc agent.ToolContext, d Deps, u *url.URL, target string) (string, error) {
	text, derr := fetchReadable(tc, d.Guarded, target)
	if derr == nil && len(text) >= minUsefulText && !looksLikeBotWall(text) {
		return text, nil
	}

	// The direct GET was thin (likely JS-rendered), failed, or hit an
	// anti-bot wall; try the crawl4ai render backend, which renders with a
	// real browser and returns clean Markdown. crawl4ai fetches the URL
	// itself with no SSRF guard, so re-check that the host doesn't resolve
	// into a blocked range before handing it over — ValidateURL above only
	// catches literal IPs, not hostnames pointing at private/metadata IPs.
	var rendered string
	var rerr error
	if d.Crawl4AI != "" {
		if rerr = validateResolvedHost(tc, u.Hostname()); rerr == nil {
			rendered, rerr = crawl4aiMarkdown(tc, d.Client, d.Crawl4AI, target)
			if rerr == nil && strings.TrimSpace(rendered) != "" && !looksLikeBotWall(rendered) {
				return rendered, nil
			}
		}
	}

	// If either attempt clearly landed on a bot wall, say so rather than
	// handing the agent CAPTCHA boilerplate as if it were the page.
	if looksLikeBotWall(text) || looksLikeBotWall(rendered) || errors.Is(derr, errCloudflareChallenge) {
		return "", fmt.Errorf("web_fetch: %s is behind an anti-bot wall (CAPTCHA / JS challenge); its content can't be read — try a different source", target)
	}

	// Never return an empty string silently — the agent needs to know the
	// fetch yielded nothing so it can try another source. Prefer a thin but
	// non-empty direct result; otherwise surface why we got nothing.
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	switch {
	case derr != nil && rerr != nil:
		return "", fmt.Errorf("web_fetch: %s unreadable: direct GET failed (%v); render failed (%v)", target, derr, rerr)
	case derr != nil:
		return "", fmt.Errorf("web_fetch: %s: %w", target, derr)
	default:
		return "", fmt.Errorf("web_fetch: %s returned no readable text (it may require login, block automated access, or have no textual content)", target)
	}
}

// sanitizeFetched makes fetched content safe to embed in session events.
// Binary payloads (e.g. an image body served where text was expected) must
// never reach the session store: invalid UTF-8 / NUL bytes make Postgres
// reject the event append (SQLSTATE 22P05), which kills the worker run
// mid-loop and surfaces as a silently-empty node. Mostly-binary content is
// rejected with a useful error; isolated bad bytes are stripped.
func sanitizeFetched(target, s string) (string, error) {
	clean := strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if len(s) > 512 && len(clean) < len(s)*9/10 {
		return "", fmt.Errorf("web_fetch: %s returned binary (non-text) content — it cannot be read as a page; try a different source", target)
	}
	return clean, nil
}

// fetchReadable does a guarded GET and returns the page's readable text.
func fetchReadable(ctx context.Context, client *http.Client, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("web_fetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", fetchAccept)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: request: %w", err)
	}
	defer resp.Body.Close()
	// Cloudflare flags a bot challenge with this header on a 403; name it so the
	// caller can report an anti-bot wall rather than a bare "403".
	if resp.StatusCode == http.StatusForbidden && strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return "", errCloudflareChallenge
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("web_fetch: got %s", resp.Status)
	}
	return readableBody(resp.Header.Get("Content-Type"), resp.Body)
}

// crawl4aiMarkdown asks the crawl4ai backend to fetch + render the page (real
// browser) and return it as Markdown. crawl4ai is a trusted internal host (plain
// client); the URL was already SSRF-validated by the caller. It uses the "fit"
// content filter (Readability-based, drops chrome) and falls back to the raw DOM
// markdown if fit prunes the page to nothing.
func crawl4aiMarkdown(ctx context.Context, client *http.Client, backend, target string) (string, error) {
	md, err := crawl4aiMD(ctx, client, backend, target, "fit")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(md) == "" {
		if md, err = crawl4aiMD(ctx, client, backend, target, "raw"); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(md), nil
}

// crawl4aiMD calls crawl4ai's POST /md endpoint with the given content filter and
// returns the Markdown it produced.
func crawl4aiMD(ctx context.Context, client *http.Client, backend, target, filter string) (string, error) {
	body, err := json.Marshal(map[string]any{"url": target, "f": filter})
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(backend, "/") + "/md"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("crawl4ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("crawl4ai: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("crawl4ai: got %s", resp.Status)
	}
	var parsed struct {
		Markdown string `json:"markdown"`
		Success  bool   `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("crawl4ai: decode response: %w", err)
	}
	if !parsed.Success {
		return "", fmt.Errorf("crawl4ai: backend reported failure for %s", target)
	}
	return parsed.Markdown, nil
}

// readableBody extracts content from an HTTP body: HTML is converted to Markdown
// (preserving links + structure) so the agent can cite the URLs a page links to;
// other content types are returned raw (capped).
func readableBody(contentType string, r io.Reader) (string, error) {
	limited := io.LimitReader(r, maxFetchBytes)
	if contentType != "" && !strings.Contains(contentType, "html") && !strings.Contains(contentType, "xml") {
		raw, err := io.ReadAll(limited)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return htmlToMarkdown(limited)
}

// markdownConverter turns HTML into Markdown. The base plugin drops
// script/style/head/iframe/noscript; we additionally drop page chrome
// (nav/header/footer/aside) so the output is closer to the article. Links and
// headings are preserved, which is the point — the researcher cites them.
var markdownConverter = newMarkdownConverter()

func newMarkdownConverter() *converter.Converter {
	conv := converter.NewConverter(
		converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin()),
	)
	for _, tag := range []string{"nav", "header", "footer", "aside"} {
		conv.Register.TagType(tag, converter.TagTypeRemove, converter.PriorityStandard)
	}
	return conv
}

// htmlToMarkdown reads HTML and returns it as Markdown.
func htmlToMarkdown(r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("web_fetch: read body: %w", err)
	}
	md, err := markdownConverter.ConvertString(string(raw))
	if err != nil {
		return "", fmt.Errorf("web_fetch: html to markdown: %w", err)
	}
	return strings.TrimSpace(md), nil
}

// botWallMarkers are telltale phrases from anti-bot interstitials (Cloudflare,
// reddit's verification wall, generic CAPTCHA gates). When short readable text is
// dominated by one of these, we landed on the challenge page, not the content.
var botWallMarkers = []string{
	"performing security verification",
	"security service to protect against malicious bots",
	"checking your browser before accessing",
	"enable javascript and cookies to continue",
	"please wait for verification",
	"verify you are human",
	"verify you are not a robot",
	"you've been blocked",
	"ray id:",
}

// looksLikeBotWall reports whether text is an anti-bot interstitial rather than
// real content. It only fires on short text — a long article that happens to
// mention one of these phrases is almost certainly genuine content.
func looksLikeBotWall(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 2000 {
		return false
	}
	low := strings.ToLower(t)
	for _, m := range botWallMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
