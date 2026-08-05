package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	minUsefulText       = 200
	maxFetchBytes       = 200_000
	fetchHeadLines      = 120
	fetchGrepMaxLines   = 120
	fetchReturnMaxBytes = 24_000
	maxTokenChars       = 4_000
	browserUA           = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	fetchAccept = "text/markdown;q=1.0, text/html;q=0.9, text/plain;q=0.8, */*;q=0.1"
)

// errCloudflareChallenge: signals a Cloudflare bot challenge (403 + cf-mitigated).
var errCloudflareChallenge = errors.New("web_fetch: cloudflare challenge (cf-mitigated)")

// dataURIRe: matches inline data URIs that HTML→markdown preserves verbatim (context garbage).
var dataURIRe = regexp.MustCompile(`data:[a-zA-Z0-9.+-]+/[a-zA-Z0-9.+-]+[;,][^\s)"'<>]*`)

type fetchArgs struct {
	URL     string `json:"url"`
	Pattern string `json:"pattern,omitempty"`
	Offset  int    `json:"offset,omitempty"`
}

// fetcher: retrieves readable text for an already-validated URL (direct or crawl4ai).
type fetcher interface {
	fetch(tc agent.Context, d Deps, u *url.URL, target string) (string, error)
}

// directFetcher: plain guarded GET, no render fallback.
type directFetcher struct{}

func (directFetcher) fetch(tc agent.Context, d Deps, u *url.URL, target string) (string, error) {
	return fetchVia(tc, d, nil, u, target)
}

// crawl4aiFetcher: direct GET with crawl4ai render fallback.
type crawl4aiFetcher struct{ renderer PageRenderer }

func (f crawl4aiFetcher) fetch(tc agent.Context, d Deps, u *url.URL, target string) (string, error) {
	return fetchVia(tc, d, f.renderer, u, target)
}

// newFetcher: selects web_fetch implementation (direct or crawl4ai).
func newFetcher(kind, base string, client *http.Client) (fetcher, error) {
	switch kind {
	case "", backendDirect:
		return directFetcher{}, nil
	case backendCrawl4AI:
		if base == "" {
			return nil, fmt.Errorf("web_fetch: kind crawl4ai requires a URL (use kind: direct for a plain GET with no backend)")
		}
		return crawl4aiFetcher{renderer: &crawl4aiRenderer{client: client, base: strings.TrimRight(base, "/")}}, nil
	default:
		return nil, fmt.Errorf("web_fetch: unknown backend kind %q", kind)
	}
}

// newFetch: builds fetch tool over a config-selected fetcher.
func newFetch(d Deps) (tool.Tool, error) {
	f, err := newFetcher(d.Fetch.Kind, d.Fetch.URL, d.Client)
	if err != nil {
		return nil, err
	}
	desc := "Fetch a web page by URL and return its readable text. "
	if _, ok := f.(crawl4aiFetcher); ok {
		desc += "Falls back to a headless browser for JavaScript-rendered pages. "
	}
	desc += "Long pages return only a head by default; the FULL page is retained, so pass `pattern` (a regex) to return just the matching lines, or `offset` (a line number) to read a window further down. Re-call the same URL with pattern/offset to drill in without re-paying the fetch."

	return functiontool.New[fetchArgs, string](
		functiontool.Config{
			Name:        "web_fetch",
			Description: desc,
		},
		func(tc agent.Context, a fetchArgs) (string, error) {
			u, err := ValidateURL(strings.TrimSpace(a.URL))
			if err != nil {
				return "", err
			}
			target := u.String()

			// Ensure full page is in cache; fetch on miss.
			var full string
			if d.Cache != nil {
				if cached, ok := d.Cache.Get(target); ok {
					full = cached
				}
			}
			if full == "" {
				fetched, ferr := f.fetch(tc, d, u, target)
				if ferr != nil {
					return "", ferr
				}
				if fetched, ferr = sanitizeFetched(target, fetched); ferr != nil {
					return "", ferr
				}
				// Cap cached copy to bound memory.
				if len(fetched) > maxFetchBytes {
					fetched = strings.ToValidUTF8(fetched[:maxFetchBytes], "") + "\n[content truncated at fetch limit]"
				}
				full = fetched
				if d.Cache != nil {
					d.Cache.Set(target, full)
				}
			}

			return shapeFetchResult(full, a.Pattern, a.Offset), nil
		},
	)
}

// shapeFetchResult: returns grep matches, offset window, or head of cached page.
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

// windowPage: returns a window of lines with navigation footer.
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

// grepPage: returns matching lines with line numbers, literal-substring fallback for invalid regex.
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
		footer = fmt.Sprintf("\n\n[first %d matches shown (more exist) - narrow the pattern, or offset=N to read around one.]", fetchGrepMaxLines)
	}
	return capFetchReturn(strings.Join(matches, "\n")) + footer
}

// capFetchReturn: hard-bounds return body to prevent context flooding.
func capFetchReturn(s string) string {
	if len(s) <= fetchReturnMaxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:fetchReturnMaxBytes], "") + "\n[…truncated; narrow your grep or use offset=N]"
}

// fetchVia: shared fetch engine - tries direct GET, falls back to render backend.
func fetchVia(ctx context.Context, d Deps, renderer PageRenderer, u *url.URL, target string) (string, error) {
	text, derr := fetchReadable(ctx, d.Guarded, target)
	if derr == nil && len(text) >= minUsefulText && !looksLikeBotWall(text) {
		return text, nil
	}

	// Direct GET failed or thin; try render backend (SSRF re-check for hostnames).
	var rendered string
	var rerr error
	if renderer != nil {
		if rerr = validateResolvedHost(ctx, u.Hostname()); rerr == nil {
			rendered, rerr = renderer.Render(ctx, target)
			if rerr == nil && strings.TrimSpace(rendered) != "" && !looksLikeBotWall(rendered) {
				return rendered, nil
			}
		}
	}

	// Bot wall: report it rather than returning CAPTCHA as page content.
	if looksLikeBotWall(text) || looksLikeBotWall(rendered) || errors.Is(derr, errCloudflareChallenge) {
		return "", fmt.Errorf("web_fetch: %s is behind an anti-bot wall (CAPTCHA / JS challenge); its content can't be read - try a different source", target)
	}

	// Never return empty silently - prefer thin direct result over nothing.
	if strings.TrimSpace(text) != "" {
		return text, nil
	}

	// Graceful degradation: render failure on a reachable target logs and returns a "render unavailable" placeholder.
	if renderer != nil && rerr != nil && derr == nil {
		slog.Warn("web_fetch: render backend failed; degrading to render-unavailable result",
			"component", "tools", "url", target, "error", rerr)
		return fmt.Sprintf("[web_fetch: render backend could not retrieve %s (%v). "+
			"The page reached its server but returned no readable text without a browser render, "+
			"and the render backend failed. Treat this source as unavailable and try another.]",
			target, rerr), nil
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

// sanitizeFetched: strips bad bytes, rejects binary content (Postgres rejects invalid UTF-8/NUL).
func sanitizeFetched(target, s string) (string, error) {
	clean := strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if len(s) > 512 && len(clean) < len(s)*9/10 {
		return "", fmt.Errorf("web_fetch: %s returned binary (non-text) content - it cannot be read as a page; try a different source", target)
	}
	return stripInlineMedia(clean), nil
}

// stripInlineMedia: removes data URIs and long tokens that binary check misses.
func stripInlineMedia(s string) string {
	s = dataURIRe.ReplaceAllString(s, "[inline-data-uri removed]")
	return collapseLongTokens(s)
}

// collapseLongTokens: replaces whitespace-free runs > maxTokenChars with a placeholder (RE2 can't match 4000).
func collapseLongTokens(s string) string {
	if len(s) <= maxTokenChars {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	runStart := -1
	flush := func(end int) {
		if runStart < 0 {
			return
		}
		if end-runStart > maxTokenChars {
			b.WriteString("[long token removed]")
		} else {
			b.WriteString(s[runStart:end])
		}
		runStart = -1
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			flush(i)
			b.WriteByte(s[i])
		default:
			if runStart < 0 {
				runStart = i
			}
		}
	}
	flush(len(s))
	return b.String()
}

// fetchReadable: guarded GET returning readable page text.
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
	// Cloudflare bot challenge header on 403.
	if resp.StatusCode == http.StatusForbidden && strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return "", errCloudflareChallenge
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("web_fetch: got %s", resp.Status)
	}
	return readableBody(resp.Header.Get("Content-Type"), resp.Body)
}

// readableBody: HTML→Markdown, other content types returned raw.
func readableBody(contentType string, r io.Reader) (string, error) {
	ct := strings.ToLower(contentType)
	if isUnreadableContentType(ct) {
		return "", fmt.Errorf("web_fetch: content-type %q is not a readable page (image/video/audio/binary) - try a different source", contentType)
	}
	limited := io.LimitReader(r, maxFetchBytes)
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml") {
		raw, err := io.ReadAll(limited)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return htmlToMarkdown(limited)
}

// isUnreadableContentType: rejects binary/media payloads.
func isUnreadableContentType(ct string) bool {
	for _, p := range []string{"image/", "video/", "audio/", "font/", "application/octet-stream", "application/zip", "application/x-"} {
		if strings.Contains(ct, p) {
			return true
		}
	}
	return false
}

// markdownConverter: HTML→Markdown, drops script/style/chrome, preserves links.
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

// htmlToMarkdown: HTML→Markdown conversion.
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

// botWallMarkers: phrases identifying anti-bot interstitials.
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

// looksLikeBotWall: anti-bot interstitial detection (only fires on short text).
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
