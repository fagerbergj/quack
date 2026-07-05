package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Outcome statuses for a non-interactive turn — the vocabulary shared by `-p`,
// `chat send`, and `chat show -f`. Deliberately mirrors schema.ChatStatus
// (running/needs_input/failed/idle) minus "running": a send blocks until its
// own run ends, so it never observes itself as running.
const (
	StatusCompleted  = "completed"
	StatusNeedsInput = "needs_input"
	StatusFailed     = "failed"
)

// getUserChoiceTool mirrors tools.ChoiceToolName (internal/tools) — duplicated
// as a literal rather than imported so the CLI stays decoupled from the
// server's internal packages (it only ever talks to the server over HTTP+SSE).
const getUserChoiceTool = "get_user_choice"

// SendResult is the observable outcome of one non-interactive turn. Exactly one
// of Answer/Question/Error is meaningful, selected by Status. This is also the
// --json shape on all three call sites (`-p`, `chat send`, `chat show -f`), so
// a scripted agent gets one object shape regardless of entry point.
type SendResult struct {
	ChatID   string `json:"chat_id"`
	Status   string `json:"status"`
	Answer   string `json:"answer,omitempty"`
	Question string `json:"question,omitempty"`
	Error    string `json:"error,omitempty"`
}

// streamState accumulates a run's observable outcome as its SSE events arrive.
// Shared by send (POST /responses, callback-style) and follow (GET /stream via
// Subscribe, channel-style) so both classify completed/needs_input/failed
// identically — the bulletproof-CLI spec's one place for "what happened".
type streamState struct {
	err       error
	orch      strings.Builder // orchestrator's own streamed answer (node_id == "")
	nodeOut   map[string]string
	successor map[string]bool // node_id that some other node depends on
	lastNode  string          // last node to finish (terminal completes last)
	question  string          // set once a pause is observed
}

func newStreamState() *streamState {
	return &streamState{nodeOut: map[string]string{}, successor: map[string]bool{}}
}

// handle folds one SSE event into the accumulated state. events, when non-nil,
// gets a compact per-event trace line (the --events pipeline trace).
func (s *streamState) handle(ev SSEEvent, events io.Writer) {
	if events != nil {
		data := string(ev.Data)
		if len(data) > 200 {
			data = data[:200] + "…"
		}
		fmt.Fprintf(events, "  «%s» %s\n", ev.Name, data)
	}
	switch ev.Name {
	case "dag_plan":
		var d struct {
			Edges []struct{ From, To string } `json:"edges"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			for _, e := range d.Edges {
				s.successor[e.From] = true
			}
		}
	case "agent_token":
		var d struct {
			NodeID string `json:"node_id"`
			Text   string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID == "" && d.Text != "" {
			s.orch.WriteString(d.Text)
		}
	case "node_done":
		var d struct {
			NodeID        string `json:"node_id"`
			Output        string `json:"output"`
			OutputPreview string `json:"output_preview"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID != "" {
			o := d.Output
			if o == "" {
				o = d.OutputPreview
			}
			s.nodeOut[d.NodeID] = o
			s.lastNode = d.NodeID
		}
	case "node_needs_input":
		var d struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Message != "" {
			s.question = d.Message
		}
	case "agent_tool_call":
		var d struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Name == getUserChoiceTool {
			if q, ok := d.Args["question"].(string); ok && q != "" {
				s.question = q
			}
		}
	case "error":
		var d struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.err = fmt.Errorf("server error: %s", d.Error)
	}
}

// result classifies the accumulated state into a SendResult: failed (a stream
// error) beats needs_input (a pause) beats completed (an answer). The answer
// preference mirrors the original PrintPrompt logic: the terminal DAG node's
// output when a plan ran, falling back to any no-successor node with output,
// and only then the orchestrator's own reply (a direct, no-DAG answer).
func (s *streamState) result(chatID string) SendResult {
	if s.err != nil {
		return SendResult{ChatID: chatID, Status: StatusFailed, Error: s.err.Error()}
	}
	if s.question != "" {
		return SendResult{ChatID: chatID, Status: StatusNeedsInput, Question: s.question}
	}
	answer := strings.TrimSpace(s.nodeOut[s.lastNode])
	if answer == "" {
		for id, o := range s.nodeOut {
			if !s.successor[id] && strings.TrimSpace(o) != "" {
				answer = strings.TrimSpace(o)
				break
			}
		}
	}
	if answer == "" {
		answer = strings.TrimSpace(s.orch.String())
	}
	return SendResult{ChatID: chatID, Status: StatusCompleted, Answer: answer}
}

// send drives one non-interactive turn against an existing chatID and
// classifies the result. Shared by RunChatSend (`chat send`) and PrintPrompt
// (`-p`, after CreateChat) — the ONE place both determine "what happened".
func send(ctx context.Context, c *Client, chatID, content string, attachPaths []string, events io.Writer) SendResult {
	st := newStreamState()
	onEvent := func(ev SSEEvent) error {
		st.handle(ev, events)
		return nil
	}
	var err error
	if len(attachPaths) > 0 {
		err = c.SendMessageWithFiles(ctx, chatID, content, attachPaths, onEvent)
	} else {
		err = c.SendMessage(ctx, chatID, content, onEvent)
	}
	if err != nil {
		return SendResult{ChatID: chatID, Status: StatusFailed, Error: err.Error()}
	}
	return st.result(chatID)
}

// Report writes a SendResult per the CLI's bulletproof pause/failure semantics
// and returns the process exit code: 0 completed, 1 failed, 2 needs_input.
// asJSON writes one JSON object to out instead of the human-readable lines —
// same exit codes either way, so a scripted caller can rely on the code alone.
func Report(out, errOut io.Writer, chatID string, r SendResult, asJSON bool) int {
	if asJSON {
		_ = writeJSON(out, r)
		return exitCode(r.Status)
	}
	switch r.Status {
	case StatusFailed:
		fmt.Fprintln(errOut, r.Error)
	case StatusNeedsInput:
		fmt.Fprintf(out, "question: %s\n", r.Question)
		fmt.Fprintf(errOut, "answer with: quack chat send %s \"...\"\n", chatID)
	default:
		if r.Answer != "" {
			fmt.Fprintln(out, r.Answer)
		}
	}
	return exitCode(r.Status)
}

func exitCode(status string) int {
	switch status {
	case StatusFailed:
		return 1
	case StatusNeedsInput:
		return 2
	default:
		return 0
	}
}

// RunChatSend is `quack chat send <id> "<msg>"`: a non-interactive turn on an
// existing chat, streaming the final answer to stdout. This is how an agent
// answers a needs_input question (the server routes a plain-text turn to the
// paused node) or asks a follow-up — no TUI required. showEvents routes the
// pipeline trace to errOut; asJSON emits one SendResult object instead of the
// human-readable lines. Returns the process exit code (see Report).
func RunChatSend(ctx context.Context, out, errOut io.Writer, server, id, content string, attachPaths []string, showEvents, asJSON bool) int {
	c, err := NewClient(server)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	var events io.Writer
	if showEvents {
		events = errOut
	}
	res := send(ctx, c, id, content, attachPaths, events)
	return Report(out, errOut, id, res, asJSON)
}

// PrintPrompt is `quack -p`: create a chat, send the prompt, and report the
// outcome via the SAME send/Report path `chat send` uses. The new chat's id is
// printed to errOut (`chat: <id>`) so stdout stays answer-only for pipes.
// Returns the process exit code (see Report).
func PrintPrompt(ctx context.Context, out, errOut, events io.Writer, server, prompt string, attachPaths []string, asJSON bool) int {
	c, err := NewClient(server)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(errOut, "chat: %s\n", chatID)
	res := send(ctx, c, chatID, prompt, attachPaths, events)
	return Report(out, errOut, chatID, res, asJSON)
}
