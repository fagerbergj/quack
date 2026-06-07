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
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

const (
	// minUsefulText: a direct GET returning less readable text than this is
	// treated as "probably JS-rendered" and retried via the browser backend.
	minUsefulText = 200
	// maxFetchBytes caps how much we read and return, to protect the agent's
	// context window and our memory.
	maxFetchBytes = 200_000
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
}

// newFetch builds the fetch tool: SSRF-guard the URL, GET it with the guarded
// client, and fall back to the crawl4ai render backend for pages a bare GET
// can't read (JS-heavy or thin).
func newFetch(d Deps) (tool.Tool, error) {
	return functiontool.New[fetchArgs, string](
		functiontool.Config{
			Name:        "web_fetch",
			Description: "Fetch a web page by URL and return its readable text. Falls back to a headless browser for JavaScript-rendered pages.",
		},
		func(tc tool.Context, a fetchArgs) (string, error) {
			u, err := ValidateURL(strings.TrimSpace(a.URL))
			if err != nil {
				return "", err
			}
			target := u.String()

			if d.Cache != nil {
				if cached, ok := d.Cache.Get(tc, target); ok {
					return cached, nil
				}
			}

			result, ferr := fetchBest(tc, d, u, target)
			if ferr != nil {
				return "", ferr
			}
			if d.Cache != nil {
				d.Cache.Set(tc, target, result, tc.SessionID(), tc.AppName())
			}
			return result, nil
		},
	)
}

// fetchBest tries to get the best readable text for target. It first tries a
// direct GET; if that is thin, failed, or bot-walled, it falls back to crawl4ai.
func fetchBest(tc tool.Context, d Deps, u *url.URL, target string) (string, error) {
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
