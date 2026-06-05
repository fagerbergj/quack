// Package rest implements the generated OpenAPI ServerInterface for Quack's
// REST surface: chat CRUD plus the streaming messages endpoint.
package rest

import (
	"encoding/json"
	"net/http"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// userID is the fixed M0 user (no auth yet).
const userID = "local"

// Handler implements schema.ServerInterface backed by the store + orchestrator.
type Handler struct {
	store *store.Store
	orch  *orchestrator.Orchestrator
}

// NewHandler builds a REST handler.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator) *Handler {
	return &Handler{store: s, orch: o}
}

// HealthCheck returns 200 "ok".
func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ListChats returns all chats.
func (h *Handler) ListChats(w http.ResponseWriter, r *http.Request) {
	chats, err := h.store.ListChats(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := schema.ChatList{Data: make([]schema.ChatSummary, 0, len(chats))}
	for _, c := range chats {
		out.Data = append(out.Data, toSummary(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateChat creates a chat (optional system prompt).
func (h *Handler) CreateChat(w http.ResponseWriter, r *http.Request) {
	var body schema.CreateChatBody
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	sp := ""
	if body.SystemPrompt != nil {
		sp = *body.SystemPrompt
	}
	c, err := h.store.CreateChat(r.Context(), sp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toSummary(*c))
}

// GetChat returns a chat with its messages projected from the ADK session.
func (h *Handler) GetChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	msgs, err := h.store.Messages(r.Context(), orchestrator.AppName, userID, chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	detail := schema.ChatDetail{
		Id:           c.ID,
		SystemPrompt: c.SystemPrompt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
		Messages:     make([]schema.ChatMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		detail.Messages = append(detail.Messages, schema.ChatMessage{
			Role:    schema.ChatMessageRole(m.Role),
			Content: m.Content,
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

// DeleteChat removes a chat.
func (h *Handler) DeleteChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if err := h.store.DeleteChat(r.Context(), chatID); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SendChatMessage runs the orchestrator and streams the response as SSE.
func (h *Handler) SendChatMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	var body schema.SendMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	for ev, err := range h.orch.Run(ctx, userID, chatID, body.Content) {
		if err != nil {
			_ = sse.send(stream.Errorf(err.Error()))
			_ = sse.send(stream.Done())
			return
		}
		for _, se := range stream.Translate(ev) {
			if sendErr := sse.send(se); sendErr != nil {
				return // client disconnected
			}
		}
	}
	_ = sse.send(stream.Done())
	_ = h.store.Touch(ctx, chatID)
}

func toSummary(c store.Chat) schema.ChatSummary {
	return schema.ChatSummary{
		Id:           c.ID,
		SystemPrompt: c.SystemPrompt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}
