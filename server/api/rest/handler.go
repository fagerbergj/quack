package rest

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct {
	researcherService ResearcherService
	chatService       ChatService
}

func NewHandler(researcherService ResearcherService, chatService ChatService) *Handler {
	return &Handler{
		researcherService: researcherService,
		chatService:       chatService,
	}
}

func (h *Handler) RegisterRoutes(r *chi.Mux) {
	r.Get("/health", h.healthCheck)
	r.Post("/api/v1/research", h.research)
	r.Get("/api/v1/chats", h.listChats)
	r.Post("/api/v1/chats", h.createChat)
	r.Get("/api/v1/chats/{chat_id}", h.getChat)
	r.Delete("/api/v1/chats/{chat_id}", h.deleteChat)
	r.Post("/api/v1/chats/{chat_id}/messages", h.sendMessage)
}

func (h *Handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) research(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	result, err := h.researcherService.Research(ctx, req.Prompt)
	if err != nil {
		http.Error(w, "Research failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(result))
}

func (h *Handler) listChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chats, err := h.chatService.ListChats(ctx)
	if err != nil {
		http.Error(w, "Failed to list chats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		Data []struct {
			ID           string `json:"id"`
			SystemPrompt string `json:"system_prompt,omitempty"`
			CreatedAt    string `json:"created_at"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"data"`
	}{
		Data: make([]struct {
			ID           string `json:"id"`
			SystemPrompt string `json:"system_prompt,omitempty"`
			CreatedAt    string `json:"created_at"`
			UpdatedAt    string `json:"updated_at"`
		}, len(chats)),
	}
	for i, chat := range chats {
		response.Data[i] = struct {
			ID           string `json:"id"`
			SystemPrompt string `json:"system_prompt,omitempty"`
			CreatedAt    string `json:"created_at"`
			UpdatedAt    string `json:"updated_at"`
		}{
			ID:           chat.ID,
			SystemPrompt: chat.SystemPrompt,
			CreatedAt:    chat.CreatedAt,
			UpdatedAt:    chat.UpdatedAt,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) createChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SystemPrompt *string `json:"system_prompt,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	session, err := h.chatService.CreateChat(ctx, req.SystemPrompt)
	if err != nil {
		http.Error(w, "Failed to create chat: "+err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		ID           string `json:"id"`
		SystemPrompt string `json:"system_prompt,omitempty"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
	}{
		ID:           session.ID,
		SystemPrompt: session.SystemPrompt,
		CreatedAt:    session.CreatedAt,
		UpdatedAt:    session.UpdatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) getChat(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	ctx := r.Context()

	session, err := h.chatService.GetChat(ctx, chatID)
	if err != nil {
		http.Error(w, "Chat not found", http.StatusNotFound)
		return
	}

	messages, err := h.chatService.GetMessages(ctx, chatID)
	if err != nil {
		http.Error(w, "Failed to get messages: "+err.Error(), http.StatusInternalServerError)
		return
	}

	messageSchemas := make([]struct {
		ID        string `json:"id"`
		Role      string `json:"role"`
		Content   string `json:"content"`
		CreatedAt string `json:"created_at"`
	}, len(messages))
	for i, msg := range messages {
		messageSchemas[i] = struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		}{
			ID:        msg.ID,
			Role:      msg.Role,
			Content:   msg.Content,
			CreatedAt: msg.CreatedAt,
		}
	}

	response := struct {
		ID           string `json:"id"`
		SystemPrompt string `json:"system_prompt,omitempty"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		Messages     []struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		} `json:"messages"`
	}{
		ID:           session.ID,
		SystemPrompt: session.SystemPrompt,
		CreatedAt:    session.CreatedAt,
		UpdatedAt:    session.UpdatedAt,
		Messages:     messageSchemas,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) deleteChat(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	ctx := r.Context()

	if err := h.chatService.DeleteChat(ctx, chatID); err != nil {
		http.Error(w, "Failed to delete chat: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")

	var req struct {
		Content string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	if _, err := h.chatService.AddMessage(ctx, chatID, "user", req.Content); err != nil {
		http.Error(w, "Failed to add message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	result, err := h.researcherService.Research(ctx, req.Content)
	if err != nil {
		http.Error(w, "Research failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := h.chatService.AddMessage(ctx, chatID, "assistant", result); err != nil {
		http.Error(w, "Failed to add message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write([]byte("event: token\ndata: " + result + "\n\n"))
	w.Write([]byte("event: done\ndata: {}\n\n"))
	w.(http.Flusher).Flush()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
