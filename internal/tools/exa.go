package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// defaultExaMCP is Exa's hosted, keyless MCP endpoint (the no-key fallback).
	defaultExaMCP = "https://mcp.exa.ai/mcp"
	// exaRESTSearch is Exa's REST search endpoint (used when an API key is set).
	exaRESTSearch = "https://api.exa.ai/search"
	// exaNumResults caps how many hits we request per query.
	exaNumResults = 6
	// exaSnippetMax caps a result's snippet length.
	exaSnippetMax = 500
)

// exaSearcher is the WebSearcher adapter for Exa. With an API key it uses the REST
// API (structured JSON — robust); without one it falls back to Exa's keyless hosted
// MCP and parses its text output. Either path satisfies the same port, so the
// agent only ever sees web_search.
type exaSearcher struct {
	apiKey       string
	client       *http.Client
	restEndpoint string // overridable in tests; defaults to Exa's REST search URL
}

func newExaSearcher(apiKey string, client *http.Client) *exaSearcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &exaSearcher{apiKey: apiKey, client: client, restEndpoint: exaRESTSearch}
}

func (e *exaSearcher) Search(ctx context.Context, query string) ([]SearchResult, string, error) {
	if e.apiKey != "" {
		return e.searchREST(ctx, query)
	}
	return e.searchMCP(ctx, query)
}

// --- REST (keyed: structured JSON) ---

func (e *exaSearcher) searchREST(ctx context.Context, query string) ([]SearchResult, string, error) {
	body, err := json.Marshal(map[string]any{
		"query":      query,
		"type":       "auto",
		"numResults": exaNumResults,
		"contents":   map[string]any{"highlights": true},
	})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.restEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("web_search: exa REST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, "", fmt.Errorf("web_search: exa REST got %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return parseExaREST(resp.Body)
}

type exaRESTResponse struct {
	Results []struct {
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		Text       string   `json:"text"`
		Highlights []string `json:"highlights"`
	} `json:"results"`
}

// parseExaREST decodes Exa's search JSON into SearchResults. The snippet is the
// joined highlights, or the text body when no highlights came back.
func parseExaREST(r io.Reader) ([]SearchResult, string, error) {
	var body exaRESTResponse
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("web_search: exa REST decode: %w", err)
	}
	out := make([]SearchResult, 0, len(body.Results))
	for _, r := range body.Results {
		if r.URL == "" {
			continue
		}
		snippet := strings.TrimSpace(strings.Join(r.Highlights, " "))
		if snippet == "" {
			snippet = r.Text
		}
		out = append(out, SearchResult{Title: r.Title, URL: r.URL, Snippet: exaSnippet(snippet)})
	}
	return out, "", nil
}

// --- MCP (keyless fallback: text output) ---

func (e *exaSearcher) searchMCP(ctx context.Context, query string) ([]SearchResult, string, error) {
	// ponytail: connects per call (no pooled session) — fine for an occasionally
	// called search tool; add a reused session if latency ever matters.
	c := mcp.NewClient(&mcp.Implementation{Name: "quack", Version: "1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: defaultExaMCP}, nil)
	if err != nil {
		return nil, "", fmt.Errorf("web_search: exa connect: %w", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "web_search_exa",
		Arguments: map[string]any{"query": query, "numResults": exaNumResults},
	})
	if err != nil {
		return nil, "", fmt.Errorf("web_search: exa call: %w", err)
	}
	if res.IsError {
		return nil, "", fmt.Errorf("web_search: exa returned an error for %q", query)
	}
	return parseExaResults(exaText(res.Content)), "", nil
}

// exaText concatenates the text parts of an MCP tool result.
func exaText(content []mcp.Content) string {
	var b strings.Builder
	for _, ct := range content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// exaRecordSep splits web_search_exa's text output into per-result blocks: records
// are separated by a line containing only "---".
var exaRecordSep = regexp.MustCompile(`(?m)^\s*---\s*$`)

// parseExaResults turns web_search_exa's text output into structured hits. Each
// block is a few "Key: value" lines (Title, URL, Published, …) followed by a
// "Highlights:" body we keep as the snippet. ponytail: this parses Exa's
// LLM-formatted text (the keyless MCP surface); the keyed path uses parseExaREST.
func parseExaResults(text string) []SearchResult {
	var out []SearchResult
	for _, block := range exaRecordSep.Split(text, -1) {
		var title, link string
		var highlights []string
		inHighlights := false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "Title:"):
				title = strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
				inHighlights = false
			case strings.HasPrefix(line, "URL:"):
				link = strings.TrimSpace(strings.TrimPrefix(line, "URL:"))
				inHighlights = false
			case strings.HasPrefix(line, "Highlights:"):
				inHighlights = true
				if rest := strings.TrimSpace(strings.TrimPrefix(line, "Highlights:")); rest != "" {
					highlights = append(highlights, rest)
				}
			case strings.HasPrefix(line, "Published:"), strings.HasPrefix(line, "Author:"),
				strings.HasPrefix(line, "ID:"), strings.HasPrefix(line, "Score:"):
				inHighlights = false
			case inHighlights:
				if t := strings.TrimSpace(line); t != "" {
					highlights = append(highlights, t)
				}
			}
		}
		if link == "" {
			continue // header/preamble block, not a result
		}
		out = append(out, SearchResult{Title: title, URL: link, Snippet: exaSnippet(strings.Join(highlights, " "))})
	}
	return out
}

// exaSnippet trims and caps a snippet string.
func exaSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > exaSnippetMax {
		s = strings.ToValidUTF8(s[:exaSnippetMax], "") + "…"
	}
	return s
}
