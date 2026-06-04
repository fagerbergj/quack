package main

import (
	"encoding/json"
	"net/http"
)

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		handleMCPGet(w, r)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func handleMCPGet(w http.ResponseWriter, r *http.Request) {
	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"researcher": map[string]interface{}{
				"url": "http://localhost:8080/api/v1/mcp",
				"type": "http",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcpConfig)
}
