// Package rest implements the generated OpenAPI ServerInterface for Quack's
// REST surface: chat CRUD plus the streaming messages endpoint.
package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// userID is the fixed M0 user (no auth yet).
const userID = "local"

// titleInstruction drives concise chat title generation.
const titleInstruction = "Generate a concise chat title (3–6 words, no punctuation, no quotes). Return only the title."

// runTimeout is the maximum time the orchestrator is allowed to run per turn,
// independent of whether the SSE client is still connected.
const runTimeout = 10 * time.Minute

// Handler implements schema.ServerInterface backed by the store + orchestrator.
type Handler struct {
	store  *store.Store
	orch   *orchestrator.Orchestrator
	titler model.LLM // used to generate chat titles; nil disables auto-titling
}

// NewHandler builds a REST handler. titler is the model used to generate short
// chat titles; pass nil to disable auto-titling.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM) *Handler {
	return &Handler{store: s, orch: o, titler: titler}
}

// generateTitle calls the titler model to produce a short chat title from the
// first user message. Returns "" on any error so callers can skip gracefully.
func (h *Handler) generateTitle(ctx context.Context, firstMessage string) string {
	if h.titler == nil {
		return ""
	}
	result, err := inference.Generate(ctx, h.titler, &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: firstMessage}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: titleInstruction}}},
			MaxOutputTokens:   20,
		},
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result)
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
		Title:        nonEmpty(c.Title),
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
//
// The orchestrator runs under a detached context so it completes even if the
// SSE client disconnects mid-stream (tab close, network drop). Events continue
// to be written to the ADK session store; the user sees the full response on
// their next visit.
//
// Title generation runs in a goroutine alongside the orchestrator run. As soon
// as the short title LLM call completes a chat_title event is injected into the
// SSE stream so the sidebar updates immediately — without waiting for done.
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

	// Detach from the HTTP request context so the orchestrator run survives a
	// client disconnect. A hard deadline prevents goroutine leaks.
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(r.Context()), runTimeout)
	defer cancelRun()

	// Start title generation concurrently. The goroutine writes the title to DB
	// and sends exactly one value on titleCh, then closes it.
	titleCh := make(chan string, 1)
	go func() {
		defer close(titleCh)
		c, _ := h.store.GetChat(runCtx, chatID)
		if c == nil || c.Title != "" {
			return
		}
		title := h.generateTitle(runCtx, body.Content)
		if title == "" {
			return
		}
		_ = h.store.UpdateTitle(runCtx, chatID, title)
		titleCh <- title
	}()

	// trySendTitle drains the title channel without blocking. Called on each SSE
	// loop iteration so the chat_title event reaches the client as soon as ready.
	clientGone := false
	trySendTitle := func() {
		select {
		case title, ok := <-titleCh:
			if ok && !clientGone {
				_ = sse.send(stream.ChatTitle(title))
			}
		default:
		}
	}

	// The whole run is the orchestrator's turn; specialist dispatches nest inside.
	// AgentEnd must balance this AgentStart on every path (including errors) or the
	// frontend's orchestrator group spins forever; it precedes Done so the group
	// closes before the stream terminates.
	_ = sse.send(stream.AgentStart(stream.OrchestratorAuthor))
	for ev, err := range h.orch.Run(runCtx, userID, chatID, body.Content) {
		trySendTitle()
		if err != nil {
			if !clientGone {
				_ = sse.send(stream.Errorf(err.Error()))
				_ = sse.send(stream.AgentEnd(stream.OrchestratorAuthor))
				_ = sse.send(stream.Done())
			}
			return
		}
		if clientGone {
			continue // drain silently so ADK stores the full response
		}
		for _, se := range stream.Translate(ev) {
			if sendErr := sse.send(se); sendErr != nil {
				clientGone = true
				break
			}
		}
	}
	// Drain: wait for the title goroutine to finish and send the event if it
	// hasn't been sent yet (e.g. orchestrator finished before title LLM returned).
	for title := range titleCh {
		if !clientGone {
			_ = sse.send(stream.ChatTitle(title))
		}
	}
	if !clientGone {
		_ = sse.send(stream.AgentEnd(stream.OrchestratorAuthor))
		_ = sse.send(stream.Done())
	}
	_ = h.store.Touch(runCtx, chatID)
}

func toSummary(c store.Chat) schema.ChatSummary {
	return schema.ChatSummary{
		Id:           c.ID,
		Title:        nonEmpty(c.Title),
		SystemPrompt: c.SystemPrompt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}
