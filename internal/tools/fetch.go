package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/recordstore"
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

	// fetchArtifactThreshold: page size at/above which a fetch stores the
	// page as an artifact instead of inlining it - today's inline return cap.
	fetchArtifactThreshold = fetchReturnMaxBytes

	// maxConcurrentFetches bounds a batched web_fetch call's in-flight requests.
	maxConcurrentFetches = 4

	// maxWindowLines caps a caller-supplied window size (read_artifact's
	// lines) so one call can't flood context.
	maxWindowLines = 500

	// maxBatchURLs bounds one web_fetch call's URL count (goroutines + worst-case context cost).
	maxBatchURLs = 20

	// maxBatchInlineBytes bounds one call's cumulative inlined text - past
	// it, a page that would otherwise inline stores as an artifact instead.
	maxBatchInlineBytes = 60_000

	kindWebPage = "web_page"

	// fetchTruncatedMarker: appended when a fetch is cut at maxFetchBytes -
	// shared so a stored header can flag it without re-deriving the suffix.
	fetchTruncatedMarker = "\n[content truncated at fetch limit]"
)

func init() {
	recordstore.Register(kindWebPage, recordstore.KindSpec{
		Class: recordstore.Blob,
		// Instance = hash of the URL (hint), not content: refetching the same
		// URL keeps landing on the same artifact id rather than minting a new one.
		Identity:     webPageIdentity,
		RequiresHint: true,
		// System: only storeWebPage may write this kind - see recordstore.KindSpec.System.
		System: true,
	})
}

// errCloudflareChallenge: signals a Cloudflare bot challenge (403 + cf-mitigated).
var errCloudflareChallenge = errors.New("web_fetch: cloudflare challenge (cf-mitigated)")

// dataURIRe: matches inline data URIs that HTML→markdown preserves verbatim (context garbage).
var dataURIRe = regexp.MustCompile(`data:[a-zA-Z0-9.+-]+/[a-zA-Z0-9.+-]+[;,][^\s)"'<>]*`)

type fetchArgs struct {
	URLs    []string `json:"urls"`
	Pattern string   `json:"pattern,omitempty"`
	Offset  int      `json:"offset,omitempty"`
}

// FetchResult: one URL's outcome in a batched call - Text is the shaped page
// or stored-artifact header; Error means only this URL failed.
type FetchResult struct {
	URL   string `json:"url"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

type fetchResponse struct {
	Results []FetchResult `json:"results"`
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

func newFetch(d Deps) (tool.Tool, error) {
	f, err := newFetcher(d.Fetch.Kind, d.Fetch.URL, d.Client)
	if err != nil {
		return nil, err
	}
	desc := fmt.Sprintf("Fetch a batch of web pages: `urls: [\"https://...\", ...]` (a single page is "+
		"still a one-element list; up to %d per call), fetched concurrently, one result entry per URL - "+
		"a failed URL is reported on its own and does not fail the rest of the batch. ", maxBatchURLs)
	if _, ok := f.(crawl4aiFetcher); ok {
		desc += "Falls back to a headless browser for JavaScript-rendered pages. "
	}
	desc += fmt.Sprintf("A short page's full text comes back inline; a page at or above %d bytes, or past "+
		"this call's combined ~%d-byte inline budget, is stored as an artifact instead and its entry is a "+
		"short header (title, url, artifact id, line/byte count, a small head) - grep_artifacts and "+
		"read_artifact(id, offset, lines) read the rest without re-fetching. `pattern` (a regex, applied to "+
		"every URL in this call) or `offset` (a line number) still shapes the full page directly as a "+
		"shortcut, storage or not.", fetchArtifactThreshold, maxBatchInlineBytes)

	return functiontool.New[fetchArgs, fetchResponse](
		functiontool.Config{
			Name:        "web_fetch",
			Description: desc,
		},
		func(tc agent.Context, a fetchArgs) (fetchResponse, error) {
			if len(a.URLs) == 0 {
				return fetchResponse{}, errors.New("web_fetch: urls must be non-empty")
			}
			if len(a.URLs) > maxBatchURLs {
				return fetchResponse{}, fmt.Errorf("web_fetch: %d urls exceeds the %d-url batch limit; split into smaller batches", len(a.URLs), maxBatchURLs)
			}
			return fetchResponse{Results: fetchBatch(tc, d, f, a.URLs, a.Pattern, a.Offset)}, nil
		},
	)
}

// fetchedPage: one URL's raw fetch outcome, before shaping.
type fetchedPage struct {
	url      string
	full     string
	cacheHit bool
	err      error
}

// fetchBatch fetches each unique URL once, shapes them against a shared
// budget, then replicates results to every position a dup URL requested.
func fetchBatch(tc agent.Context, d Deps, f fetcher, urls []string, pattern string, offset int) []FetchResult {
	order, idxsByKey, rawByKey := dedupURLs(urls)
	pages := fetchPages(tc, d, f, order, rawByKey)
	shaped := shapePages(tc, d, pages, pattern, offset)

	out := make([]FetchResult, len(urls))
	for ui, key := range order {
		for _, idx := range idxsByKey[key] {
			out[idx] = shaped[ui]
		}
	}
	return out
}

// dedupURLs: first-seen order of urls' trimmed values, with every original
// index each one covers - so a URL repeated in one batch is fetched once.
func dedupURLs(urls []string) (order []string, idxsByKey map[string][]int, rawByKey map[string]string) {
	idxsByKey = map[string][]int{}
	rawByKey = map[string]string{}
	for i, raw := range urls {
		key := strings.TrimSpace(raw)
		if _, seen := idxsByKey[key]; !seen {
			order = append(order, key)
			rawByKey[key] = raw
		}
		idxsByKey[key] = append(idxsByKey[key], i)
	}
	return order, idxsByKey, rawByKey
}

// fetchPages fetches each of order's URLs concurrently, bounded by maxConcurrentFetches.
func fetchPages(tc agent.Context, d Deps, f fetcher, order []string, rawByKey map[string]string) []fetchedPage {
	out := make([]fetchedPage, len(order))
	sem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup
	for i, key := range order {
		wg.Add(1)
		go func(i int, raw string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = fetchOnePage(tc, d, f, raw)
		}(i, rawByKey[key])
	}
	wg.Wait()
	return out
}

// fetchOnePage validates, then fetches (cache-first) and sanitizes one URL.
// Shaping/storage happen later in shapePages, so a cache hit is visible.
func fetchOnePage(tc agent.Context, d Deps, f fetcher, raw string) fetchedPage {
	u, err := ValidateURL(strings.TrimSpace(raw))
	if err != nil {
		return fetchedPage{url: raw, err: err}
	}
	target := u.String()
	if d.Cache != nil {
		if cached, ok := d.Cache.Get(target); ok {
			return fetchedPage{url: target, full: cached, cacheHit: true}
		}
	}
	fetched, ferr := f.fetch(tc, d, u, target)
	if ferr != nil {
		return fetchedPage{url: target, err: ferr}
	}
	if fetched, ferr = sanitizeFetched(target, fetched); ferr != nil {
		return fetchedPage{url: target, err: ferr}
	}
	if len(fetched) > maxFetchBytes {
		fetched = strings.ToValidUTF8(fetched[:maxFetchBytes], "") + fetchTruncatedMarker
	}
	if d.Cache != nil {
		d.Cache.Set(target, fetched)
	}
	return fetchedPage{url: target, full: fetched}
}

// shapePages shapes pages in order against a shared budget: past
// maxBatchInlineBytes cumulative, later under-threshold pages store too.
func shapePages(tc agent.Context, d Deps, pages []fetchedPage, pattern string, offset int) []FetchResult {
	out := make([]FetchResult, len(pages))
	budget := maxBatchInlineBytes
	for i, p := range pages {
		if p.err != nil {
			out[i] = FetchResult{URL: p.url, Error: p.err.Error()}
			continue
		}
		text, inlined := shapeOrStore(tc, d, p, pattern, offset, budget > 0)
		if inlined {
			budget -= len(text)
		}
		out[i] = FetchResult{URL: p.url, Text: text}
	}
	return out
}

// shapeOrStore shapes one page. allowInline false forces storage even under
// threshold (budget spent); inlined says whether text counts against it.
func shapeOrStore(tc agent.Context, d Deps, p fetchedPage, pattern string, offset int, allowInline bool) (text string, inlined bool) {
	underThreshold := len(p.full) < fetchArtifactThreshold
	var header string
	if (!underThreshold || !allowInline) && d.RecordStore != nil {
		header = storeWebPage(tc, d, p.url, p.full, p.cacheHit)
	}
	if strings.TrimSpace(pattern) != "" || offset > 0 {
		return shapeFetchResult(p.full, pattern, offset), false
	}
	if header != "" {
		return header, false
	}
	if underThreshold {
		return capFetchReturn(p.full), true
	}
	return shapeFetchResult(p.full, "", 0), false
}

// webPageIdentity: instance = a short hash of the URL (hint), ignoring
// content, so refetching the same page keeps landing on the same artifact id.
func webPageIdentity(_ []byte, hint string) (string, error) {
	if hint == "" {
		return "", errors.New("web_page: no url hint for identity")
	}
	h := sha256.Sum256([]byte(hint))
	return hex.EncodeToString(h[:])[:12], nil
}

// pageTitle: first non-blank line, as a stand-in for the page's real title.
// ponytail: no <title> tag extraction; add it if titles prove unreliable.
func pageTitle(text string) string {
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		return strings.TrimSpace(strings.TrimLeft(ln, "# "))
	}
	return ""
}

// storeWebPage persists full verbatim (or, on a cache hit, reuses its
// existing id) and returns the short header; "" falls back to an inline head.
func storeWebPage(tc agent.Context, d Deps, target, full string, cacheHit bool) string {
	title := pageTitle(full)
	truncated := strings.HasSuffix(full, fetchTruncatedMarker)
	if cacheHit {
		if id, ok := existingWebPageID(tc, d, target, full); ok {
			return webPageHeader(title, target, id, full, truncated)
		}
	}
	lineage := recordstore.Lineage{Author: "worker", SavedAt: time.Now().UTC()}
	if d.NodeID != "" {
		lineage.NodeID = d.NodeID
	}
	if d.Coords != nil {
		lineage.Round, lineage.TurnID, lineage.HeadSHA, lineage.TriggerAnnotation = d.Coords.Round, d.Coords.TurnID, d.Coords.HeadSHA, d.Coords.TriggerAnnotation
	}
	id, _, err := d.RecordStore.SaveBlob(tc, kindWebPage, []byte(full), "text/markdown", target, lineage)
	if err != nil {
		slog.Warn("web_fetch: store page artifact failed; falling back to inline head", "component", "tools", "url", target, "error", err)
		return ""
	}
	return webPageHeader(title, target, id, full, truncated)
}

// existingWebPageID: id is a pure function of the URL, so a cache hit can
// reuse it instead of writing a duplicate revision; false retries a fresh save.
func existingWebPageID(tc agent.Context, d Deps, target, full string) (string, bool) {
	id, err := recordstore.IdentityFor(kindWebPage, "", target)
	if err != nil {
		return "", false
	}
	data, _, ok, err := d.RecordStore.Latest(tc, id)
	if err != nil || !ok || string(data) != full {
		return "", false
	}
	return id, true
}

// webPageHeader: the short entry a stored page returns in place of its full text.
func webPageHeader(title, target, id, content string, truncated bool) string {
	ls := strings.Split(content, "\n")
	head := windowLines(ls, 1, fetchHeadLines, len(ls))
	note := ""
	if truncated {
		note = " (truncated at the fetch limit)"
	}
	return fmt.Sprintf("title: %s\nurl: %s\nartifact: %s\nlines: %d\nbytes: %d%s\n\n%s",
		title, target, id, len(ls), len(content), note, head)
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
	return windowLines(lines, start, fetchHeadLines, total)
}

// windowLines: returns lines[start-1:end] (1-based, end from want, capped at
// total and at maxWindowLines) plus a navigation footer.
func windowLines(lines []string, start, want, total int) string {
	if start > total {
		return fmt.Sprintf("[offset %d is past the end of this page (%d lines). Use a smaller offset or grep to search.]", start, total)
	}
	if want <= 0 {
		want = fetchHeadLines
	}
	if want > maxWindowLines {
		want = maxWindowLines
	}
	end := start + want - 1
	if end > total {
		end = total
	}
	body := strings.Join(lines[start-1:end], "\n")
	var footer string
	if end < total {
		footer = fmt.Sprintf("\n\n[lines %d–%d of %d. offset=%d to read further.]", start, end, total, end+1)
	} else {
		footer = fmt.Sprintf("\n\n[lines %d–%d of %d (end).]", start, end, total)
	}
	return capFetchReturn(body) + footer
}

// compileGrepMatcher: case-insensitive regex match, falling back to a
// literal lowercase substring match when pattern doesn't compile as regex.
func compileGrepMatcher(pattern string) func(string) bool {
	re, err := regexp.Compile("(?i)" + pattern)
	if err == nil {
		return re.MatchString
	}
	needle := strings.ToLower(strings.TrimSpace(pattern))
	return func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }
}

// grepPage: returns matching lines with line numbers, literal-substring fallback for invalid regex.
func grepPage(lines []string, pattern string) string {
	matchLine := compileGrepMatcher(pattern)
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

	return fetchFallback(target, text, rendered, derr, rerr, renderer != nil)
}

// fetchFallback decides what a failed thin fetch reports: an anti-bot wall,
// the thin direct text, a render-unavailable placeholder, or the errors.
func fetchFallback(target, text, rendered string, derr, rerr error, hadRenderer bool) (string, error) {
	// Bot wall: report it rather than returning CAPTCHA as page content.
	if looksLikeBotWall(text) || looksLikeBotWall(rendered) || errors.Is(derr, errCloudflareChallenge) {
		return "", fmt.Errorf("web_fetch: %s is behind an anti-bot wall (CAPTCHA / JS challenge); its content can't be read - try a different source", target)
	}

	// Never return empty silently - prefer thin direct result over nothing.
	if strings.TrimSpace(text) != "" {
		return text, nil
	}

	// Graceful degradation: render failure on a reachable target logs and returns a "render unavailable" placeholder.
	if hadRenderer && rerr != nil && derr == nil {
		slog.Warn("web_fetch: render backend failed; degrading to render-unavailable result",
			"component", "tools", "url", target, "error", rerr)
		return fmt.Sprintf("[web_fetch: render backend could not retrieve %s (%v). "+
			"The page reached its server but returned no readable text without a browser render, "+
			"and the render backend failed. Treat this source as unavailable and try another.]",
			target, rerr), nil
	}

	switch {
	case derr != nil && rerr != nil:
		return "", fmt.Errorf("web_fetch: %s unreadable: direct GET failed (%w); render failed (%w)", target, derr, rerr)
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
	defer func() { _ = resp.Body.Close() }()
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
