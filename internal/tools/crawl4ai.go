package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// crawl4ai settle-wait knobs: network-idle wait, settle delay, page timeout.
const (
	crawl4aiWaitUntil          = "networkidle"
	crawl4aiSettleDelaySeconds = 2.0
	crawl4aiPageTimeoutMS      = 25000
)

// crawl4aiRenderer: crawl4ai adapter for PageRenderer port. Caller SSRF-validates first.
type crawl4aiRenderer struct {
	client *http.Client
	base   string // trimmed of a trailing slash
}

func (r *crawl4aiRenderer) Render(ctx context.Context, target string) (string, error) {
	return crawl4aiMarkdown(ctx, r.client, r.base, target)
}

// crawl4aiMarkdown: fetches + renders page via crawl4ai, preferring fit markdown with raw fallback.
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

// crawl4aiCrawl: POSTs to /crawl with settle-wait config; /md is unused (ignores wait options).
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
