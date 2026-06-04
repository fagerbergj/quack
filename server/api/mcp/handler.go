package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/jasonfagerberg/agent-researcher/server/core"
)

// Handler represents the MCP API handler
type Handler struct {
	chatService core.ChatService
}

// NewHandler creates a new MCP handler
func NewHandler(chatService core.ChatService) *Handler {
	return &Handler{
		chatService: chatService,
	}
}

// RegisterRoutes registers MCP routes
func (h *Handler) RegisterRoutes(r *http.ServeMux) {
	r.HandleFunc("/api/v1/research/mcp", h.handleMCP)
}

// handleMCP handles MCP requests
func (h *Handler) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.handleMCPGet(w, r)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleMCPGet returns MCP configuration
func (h *Handler) handleMCPGet(w http.ResponseWriter, r *http.Request) {
	mcpConfig := map[string]interface{}{
		"name":        "agent-researcher",
		"version":     "0.1.0",
		"instructions": "Research agent for deep web research using LLMs and search tools",
		"tools": []map[string]interface{}{
			{
				"name":        "web_search",
				"description": "Search the web for information on a topic",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Search query",
						},
						"k": map[string]interface{}{
							"type":        "integer",
							"default":     5,
							"description": "Number of results to return",
						},
					},
					"required": []string{"query"},
				},
			},
			{
				"name":        "rag_search",
				"description": "Semantic search across your research knowledge base",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Search query",
						},
						"k": map[string]interface{}{
							"type":        "integer",
							"default":     5,
							"description": "Number of results to return",
						},
						"min_score": map[string]interface{}{
							"type":        "number",
							"default":     0.5,
							"description": "Minimum similarity score",
						},
					},
					"required": []string{"query"},
				},
			},
			{
				"name":        "summarize",
				"description": "Generate a summary of a document or text content",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "Content to summarize",
						},
						"max_length": map[string]interface{}{
							"type":        "integer",
							"default":     1000,
							"description": "Maximum length of summary",
						},
					},
					"required": []string{"content"},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcpConfig)
}
