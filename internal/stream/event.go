// Package stream defines Quack's wire-level event vocabulary and translates
// ADK session events into it. The vocabulary mirrors the frontend contract in
// frontend/src/state/agentStream.ts and is shared by the REST and MCP transports.
//
// Every activity event carries the `agent` that produced it (the ADK event
// author), and the dispatch lifecycle is explicit: `agent_start` when the
// orchestrator transfers to a specialist, `agent_end` when that specialist's turn
// completes. The frontend uses these to nest activity under the right actor
// instead of inferring boundaries from message content.
package stream

import "google.golang.org/adk/session"

// OrchestratorAuthor is the ADK author of the root dispatcher's events (mirrors
// the agent name in internal/orchestrator). Its turn-completion is its own, not a
// dispatched specialist's, so it does not emit an agent_end.
const OrchestratorAuthor = "orchestrator"

// transferTool is ADK's built-in dispatch tool; its call/response surface as the
// agent_start lifecycle event rather than as a normal tool call.
const transferTool = "transfer_to_agent"

// Event names. M0 emitted only token / done / error; the rest fill in as later
// milestones emit them.
const (
	EventToken               = "token"
	EventThinking            = "thinking"
	EventToolCall            = "tool_call"
	EventToolResult          = "tool_result"
	EventAgentStart          = "agent_start"
	EventAgentEnd            = "agent_end"
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
	Agent string `json:"agent,omitempty"`
	Text  string `json:"text"`
}

// ErrorData is the `error` event payload.
type ErrorData struct {
	Error string `json:"error"`
}

// ThinkingData is the `thinking` event payload.
type ThinkingData struct {
	Agent string `json:"agent,omitempty"`
	Text  string `json:"text"`
}

// ToolCallData is the `tool_call` event payload.
type ToolCallData struct {
	Agent string         `json:"agent,omitempty"`
	Name  string         `json:"name"`
	Args  map[string]any `json:"args"`
}

// ToolResultData is the `tool_result` event payload.
type ToolResultData struct {
	Agent  string `json:"agent,omitempty"`
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// AgentData is the payload for the agent_start / agent_end lifecycle events.
type AgentData struct {
	Agent string `json:"agent"`
}

// Token builds a token event.
func Token(agent, text string) SSEEvent {
	return SSEEvent{Name: EventToken, Data: TokenData{Agent: agent, Text: text}}
}

// Thinking builds a thinking (reasoning) event.
func Thinking(agent, text string) SSEEvent {
	return SSEEvent{Name: EventThinking, Data: ThinkingData{Agent: agent, Text: text}}
}

// ToolCall builds a tool_call event.
func ToolCall(agent, name string, args map[string]any) SSEEvent {
	return SSEEvent{Name: EventToolCall, Data: ToolCallData{Agent: agent, Name: name, Args: args}}
}

// ToolResult builds a tool_result event.
func ToolResult(agent, name string, result any) SSEEvent {
	return SSEEvent{Name: EventToolResult, Data: ToolResultData{Agent: agent, Name: name, Result: result}}
}

// AgentStart marks the orchestrator dispatching to a specialist agent.
func AgentStart(agent string) SSEEvent {
	return SSEEvent{Name: EventAgentStart, Data: AgentData{Agent: agent}}
}

// AgentEnd marks a specialist agent's turn completing.
func AgentEnd(agent string) SSEEvent {
	return SSEEvent{Name: EventAgentEnd, Data: AgentData{Agent: agent}}
}

// Errorf builds an error event.
func Errorf(msg string) SSEEvent { return SSEEvent{Name: EventError, Data: ErrorData{Error: msg}} }

// Done builds the terminal done event.
func Done() SSEEvent { return SSEEvent{Name: EventDone, Data: struct{}{}} }

// Translate maps one ADK session event to zero or more wire events. Each part is
// labeled by kind — reasoning (Thought) → thinking, function calls → tool_call,
// function responses → tool_result, plain text → token — and tagged with the
// event's author so the frontend can attribute it. A dispatch (Actions.
// TransferToAgent) emits agent_start; a specialist's turn completion (TurnComplete
// from a non-orchestrator author) emits agent_end. The built-in transfer tool's
// own call/response are suppressed in favor of those lifecycle events. The caller
// emits the terminal `done` after the stream ends.
func Translate(ev *session.Event) []SSEEvent {
	if ev == nil {
		return nil
	}
	agent := ev.Author
	var out []SSEEvent

	// A dispatch opens the specialist's group before its events arrive.
	if ev.Actions.TransferToAgent != "" {
		out = append(out, AgentStart(ev.Actions.TransferToAgent))
	}

	if ev.Content != nil {
		for _, p := range ev.Content.Parts {
			switch {
			case p == nil:
				continue
			case p.Thought && p.Text != "":
				out = append(out, Thinking(agent, p.Text))
			case p.FunctionCall != nil:
				if p.FunctionCall.Name == transferTool {
					continue // surfaced as agent_start
				}
				out = append(out, ToolCall(agent, p.FunctionCall.Name, p.FunctionCall.Args))
			case p.FunctionResponse != nil:
				if p.FunctionResponse.Name == transferTool {
					continue // surfaced as agent_start
				}
				out = append(out, ToolResult(agent, p.FunctionResponse.Name, p.FunctionResponse.Response))
			case p.Text != "":
				out = append(out, Token(agent, p.Text))
			}
		}
	}

	// A specialist completing its turn closes its group. The orchestrator's own
	// turn-completion is the run itself, closed by the caller, not a dispatch.
	if ev.TurnComplete && agent != "" && agent != OrchestratorAuthor {
		out = append(out, AgentEnd(agent))
	}
	return out
}
