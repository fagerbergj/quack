package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
// browser) and return it as Markdown. It uses the "fit" content filter
// (Readability-based, drops chrome) and falls back to the raw DOM markdown if fit
// prunes the page to nothing.
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
