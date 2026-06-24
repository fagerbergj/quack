package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultExaMCP is Exa's hosted, keyless MCP endpoint. The exa web_search adapter
// speaks MCP to it so the agent gets search with no API key and no container — the
// no-docker path. The MCP plumbing is an implementation detail of this adapter; the
// agent only ever sees the web_search tool, identical to the SearXNG backend.
const defaultExaMCP = "https://mcp.exa.ai/mcp"

// exaNumResults caps how many hits we request per query.
const exaNumResults = 6

// exaSearcher is the WebSearcher adapter backed by Exa's hosted MCP web_search_exa
// tool. ponytail: it connects per call (no pooled session) — fine for an
// occasionally-called search tool; add a reused session if latency ever matters.
type exaSearcher struct{ endpoint string }

func newExaSearcher(base string) *exaSearcher {
	if base == "" {
		base = defaultExaMCP
	}
	return &exaSearcher{endpoint: strings.TrimRight(base, "/")}
}

func (e *exaSearcher) Search(ctx context.Context, query string) ([]SearchResult, string, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "quack", Version: "1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.endpoint}, nil)
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
// LLM-formatted text (the keyless MCP surface); the keyed REST API returns JSON —
// swap to it if an API key is ever configured.
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
		out = append(out, SearchResult{Title: title, URL: link, Snippet: exaSnippet(highlights)})
	}
	return out
}

// exaSnippet joins highlight lines into one capped snippet.
func exaSnippet(lines []string) string {
	s := strings.Join(lines, " ")
	const max = 500
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}
