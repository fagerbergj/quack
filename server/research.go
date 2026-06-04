package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/genai"
)

func handleResearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Prompt == "" {
		http.Error(w, "Prompt is required", http.StatusBadRequest)
		return
	}

	model, err := genai.NewClient(context.Background())
	if err != nil {
		http.Error(w, "Failed to create AI client", http.StatusInternalServerError)
		return
	}

	defer model.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := model.GenerateContent(ctx, genai.Text(req.Prompt))
	if err != nil {
		http.Error(w, "AI generation failed", http.StatusInternalServerError)
		return
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		http.Error(w, "No response from AI", http.StatusInternalServerError)
		return
	}

	text := resp.Candidates[0].Content.Parts[0].(genai.Text)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(text))
}
