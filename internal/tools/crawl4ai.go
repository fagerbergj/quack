package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// crawl4ai settle-wait knobs. crawl4ai reads the page HTML with Playwright, and a
// page that is still redirecting / fetching / mutating its DOM makes that read race
// the navigation — the backend then 500s with "Page.content: Unable to retrieve
// content because the page is navigating and changing the content". We defend by
// telling crawl4ai to wait for the page to settle before grabbing the HTML.
const (
	// crawl4aiWaitUntil: consider the page "loaded" only once the network has gone
	// idle, so a page still redirecting or lazy-loading isn't read mid-navigation.
	crawl4aiWaitUntil = "networkidle"
	// crawl4aiSettleDelaySeconds: an extra fixed delay after load before the HTML is
	// grabbed, to let late JS mutations finish.
	crawl4aiSettleDelaySeconds = 2.0
	// crawl4aiPageTimeoutMS bounds crawl4ai's per-page work *below* the plain HTTP
	// client's 30s timeout (registry.go), so a stuck page fails fast server-side
	// (returning a clean error we can degrade on) instead of the Go client dropping
	// the connection out from under it.
	crawl4aiPageTimeoutMS = 25000
)

// crawl4aiRenderer is the crawl4ai adapter for the PageRenderer port. crawl4ai is
// a trusted internal host (plain client); the caller SSRF-validates the URL
// before calling, because crawl4ai fetches the URL itself server-side.
type crawl4aiRenderer struct {
	client *http.Client
	base   string // trimmed of a trailing slash
}

func (r *crawl4aiRenderer) Render(ctx context.Context, target string) (string, error) {
	return crawl4aiMarkdown(ctx, r.client, r.base, target)
}

// crawl4aiMarkdown asks the crawl4ai backend to fetch + render the page (real
// browser, after waiting for it to settle — see the crawl4aiWaitUntil knobs) and
// return it as Markdown. crawl4ai's /crawl returns both a "fit" markdown
// (Readability-based, drops chrome) and the raw DOM markdown in one response; we
// prefer fit and fall back to raw when fit prunes the page to nothing.
func crawl4aiMarkdown(ctx context.Context, client *http.Client, backend, target string) (string, error) {
	fit, raw, err := crawl4aiCrawl(ctx, client, backend, target)
	if err != nil {
		return "", err
	}
	md := strings.TrimSpace(fit)
	if md == "" {
		md = strings.TrimSpace(raw)
	}
	return md, nil
}

// crawl4aiCrawl POSTs to crawl4ai's /crawl endpoint with a settle-wait
// crawler_config and returns the page's fit and raw Markdown. The settle-wait
// (network-idle + a fixed delay) is what avoids reading the page mid-navigation.
// (The lighter /md endpoint is not used: this crawl4ai version silently ignores
// unknown fields on /md, so a wait option there would be a no-op — only /crawl's
// crawler_config actually takes effect.)
func crawl4aiCrawl(ctx context.Context, client *http.Client, backend, target string) (fit, raw string, err error) {
	body, err := json.Marshal(map[string]any{
		"urls": []string{target},
		"crawler_config": map[string]any{
			"type": "CrawlerRunConfig",
			"params": map[string]any{
				"wait_until":               crawl4aiWaitUntil,
				"delay_before_return_html": crawl4aiSettleDelaySeconds,
				"page_timeout":             crawl4aiPageTimeoutMS,
			},
		},
	})
	if err != nil {
		return "", "", err
	}
	endpoint := strings.TrimRight(backend, "/") + "/crawl"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("crawl4ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("crawl4ai: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("crawl4ai: got %s", resp.Status)
	}
	var parsed struct {
		Success bool `json:"success"`
		Results []struct {
			Success  bool `json:"success"`
			Markdown struct {
				RawMarkdown string `json:"raw_markdown"`
				FitMarkdown string `json:"fit_markdown"`
			} `json:"markdown"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", fmt.Errorf("crawl4ai: decode response: %w", err)
	}
	if !parsed.Success || len(parsed.Results) == 0 || !parsed.Results[0].Success {
		return "", "", fmt.Errorf("crawl4ai: backend reported failure for %s", target)
	}
	m := parsed.Results[0].Markdown
	return m.FitMarkdown, m.RawMarkdown, nil
}
