// Package stream defines Quack's wire-level event vocabulary and translates
// ADK session events into it. The vocabulary mirrors the frontend contract in
// frontend/src/state/agentStream.ts and is shared by the REST and MCP transports.
package stream

import "google.golang.org/adk/session"

// Event names. M0 emits only token / done / error; the rest are defined now so
// the contract is complete as later milestones start emitting them.
const (
	EventToken               = "token"
	EventThinking            = "thinking"
	EventToolCall            = "tool_call"
	EventToolResult          = "tool_result"
	EventConfirmationRequest = "confirmation_request"
	EventError               = "error"
	EventDone                = "done"
)

// SSEEvent is one server-sent event: a name plus a JSON-serializable payload.
type SSEEvent struct {
	Name string
	Data any
}

// TokenData is the `token` event payload.
type TokenData struct {
	Text string `json:"text"`
}

// ErrorData is the `error` event payload.
type ErrorData struct {
	Error string `json:"error"`
}

// ThinkingData is the `thinking` event payload.
type ThinkingData struct {
	Text string `json:"text"`
}

// ToolCallData is the `tool_call` event payload.
type ToolCallData struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ToolResultData is the `tool_result` event payload.
type ToolResultData struct {
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// Token builds a token event.
func Token(text string) SSEEvent { return SSEEvent{Name: EventToken, Data: TokenData{Text: text}} }

// Thinking builds a thinking (reasoning) event.
func Thinking(text string) SSEEvent {
	return SSEEvent{Name: EventThinking, Data: ThinkingData{Text: text}}
}

// ToolCall builds a tool_call event.
func ToolCall(name string, args map[string]any) SSEEvent {
	return SSEEvent{Name: EventToolCall, Data: ToolCallData{Name: name, Args: args}}
}

// ToolResult builds a tool_result event.
func ToolResult(name string, result any) SSEEvent {
	return SSEEvent{Name: EventToolResult, Data: ToolResultData{Name: name, Result: result}}
}

// Errorf builds an error event.
func Errorf(msg string) SSEEvent { return SSEEvent{Name: EventError, Data: ErrorData{Error: msg}} }

// Done builds the terminal done event.
func Done() SSEEvent { return SSEEvent{Name: EventDone, Data: struct{}{}} }

// Translate maps one ADK session event to zero or more wire events, labeling each
// part by kind: reasoning (Thought) parts become `thinking`, function calls become
// `tool_call`, function responses become `tool_result`, and plain text becomes
// `token`. The caller emits the terminal `done` after the event stream ends.
func Translate(ev *session.Event) []SSEEvent {
	if ev == nil || ev.Content == nil {
		return nil
	}
	var out []SSEEvent
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil:
			continue
		case p.Thought && p.Text != "":
			out = append(out, Thinking(p.Text))
		case p.FunctionCall != nil:
			out = append(out, ToolCall(p.FunctionCall.Name, p.FunctionCall.Args))
		case p.FunctionResponse != nil:
			out = append(out, ToolResult(p.FunctionResponse.Name, p.FunctionResponse.Response))
		case p.Text != "":
			out = append(out, Token(p.Text))
		}
	}
	return out
}
