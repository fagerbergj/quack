package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/fagerbergj/quack/internal/stream"
)

// Outcome statuses shared by `-p`, `chat send` and `chat show -f`: schema.ChatStatus minus "running",
// since a send blocks until its own run ends.
const (
	StatusCompleted  = "completed"
	StatusNeedsInput = "needs_input"
	StatusFailed     = "failed"
)

// getUserChoiceTool mirrors tools.ChoiceToolName as a literal so the CLI stays decoupled from server
// packages (it only speaks HTTP+SSE).
const getUserChoiceTool = "get_user_choice"

// SendResult is one non-interactive turn's outcome; Status selects which of Answer/Question/Error
// is meaningful. It is also the --json shape for `-p`, `chat send` and `chat show -f`.
type SendResult struct {
	ChatID   string `json:"chat_id"`
	Status   string `json:"status"`
	Answer   string `json:"answer,omitempty"`
	Question string `json:"question,omitempty"`
	Error    string `json:"error,omitempty"`
}

// streamState accumulates a run's outcome from SSE events, shared by send (POST) and follow (GET)
// so both classify completed/needs_input/failed identically.
type streamState struct {
	err       error
	orch      strings.Builder // orchestrator's own streamed answer (node_id == "")
	nodeOut   map[string]string
	successor map[string]bool // node_id that some other node depends on
	order     []string        // the plan's node ids, in plan order
	cancelled map[string]bool // node_id the user stopped
	lastNode  string          // last node to finish (terminal completes last)
	question  string          // set once a pause is observed
}

func newStreamState() *streamState {
	return &streamState{nodeOut: map[string]string{}, successor: map[string]bool{}, cancelled: map[string]bool{}}
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
		s.onDagPlan(ev.Data)
	case "node_cancelled":
		s.onNodeCancelled(ev.Data)
	case "agent_token":
		var d struct {
			NodeID string `json:"node_id"`
			Text   string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID == "" && d.Text != "" {
			s.orch.WriteString(d.Text)
		}
	case "node_done":
		s.onNodeDone(ev.Data)
	case "node_needs_input":
		var d struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Message != "" {
			s.question = d.Message
		}
	case "agent_tool_call":
		s.onToolCall(ev.Data)
	case "error":
		var d struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.err = fmt.Errorf("server error: %s", d.Error)
	}
}

func (s *streamState) onDagPlan(data json.RawMessage) {
	var d struct {
		Nodes []struct{ ID string }       `json:"nodes"`
		Edges []struct{ From, To string } `json:"edges"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	s.order = s.order[:0]
	for _, n := range d.Nodes {
		s.order = append(s.order, n.ID)
	}
	for _, e := range d.Edges {
		s.successor[e.From] = true
	}
}

func (s *streamState) onNodeCancelled(data json.RawMessage) {
	var d struct {
		NodeID string `json:"node_id"`
	}
	if json.Unmarshal(data, &d) == nil {
		s.cancelled[d.NodeID] = true
	}
}

func (s *streamState) onNodeDone(data json.RawMessage) {
	var d struct {
		NodeID        string `json:"node_id"`
		Output        string `json:"output"`
		OutputPreview string `json:"output_preview"`
	}
	if json.Unmarshal(data, &d) != nil || d.NodeID == "" {
		return
	}
	o := d.Output
	if o == "" {
		o = d.OutputPreview
	}
	s.nodeOut[d.NodeID] = o
	s.lastNode = d.NodeID
}

func (s *streamState) onToolCall(data json.RawMessage) {
	var d struct {
		NodeID string         `json:"node_id"`
		Name   string         `json:"name"`
		Args   map[string]any `json:"args"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	if d.NodeID == "" {
		// Narration before an orchestrator tool call is preamble, not its answer: the
		// CLI's final printed answer (Report) must never include it.
		s.orch.Reset()
	}
	if q, ok := d.Args["question"].(string); ok && q != "" && d.Name == getUserChoiceTool {
		s.question = q
	}
}

// result classifies the state: failed beats needs_input beats completed. The answer is the terminal DAG
// node's output, else any sink node's output, else the orchestrator's own reply.
func (s *streamState) result(chatID string) SendResult {
	if s.err != nil {
		return SendResult{ChatID: chatID, Status: StatusFailed, Error: s.err.Error()}
	}
	if s.question != "" {
		return SendResult{ChatID: chatID, Status: StatusNeedsInput, Question: s.question}
	}
	answer := s.sinkSections()
	if answer == "" {
		answer = strings.TrimSpace(s.nodeOut[s.lastNode])
	}
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

// sinkSections is a multi-sink plan's answer as the server delivers it: each sink that finished or was
// stopped as its own section, a stopped one's draft masked; "" for one sink or when every one stopped.
func (s *streamState) sinkSections() string {
	var sinks []stream.SinkAnswer
	anyLive := false
	for _, id := range s.order {
		out, done := s.nodeOut[id]
		if s.successor[id] || !done && !s.cancelled[id] {
			continue
		}
		anyLive = anyLive || !s.cancelled[id]
		sinks = append(sinks, stream.SinkAnswer{Label: id, Text: out, Stopped: s.cancelled[id]})
	}
	if len(sinks) < 2 || !anyLive {
		return ""
	}
	return stream.JoinSinkAnswers(sinks)
}

// send drives one non-interactive turn on chatID and classifies the result, for both `chat send`
// and `-p`.
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

// Report writes r (one JSON object when asJSON) and returns the exit code: 0 completed, 1 failed,
// 2 needs_input.
func Report(out, errOut io.Writer, chatID string, r SendResult, asJSON bool) int {
	if asJSON {
		_ = WriteJSON(out, r)
		return exitCode(r.Status)
	}
	switch r.Status {
	case StatusFailed:
		fmt.Fprintln(errOut, r.Error+dialFailureHint(r.Error))
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

// dialFailureHint returns a " (...)" suffix naming the config key to check,
// or "" - only text is available here (the SSE "error" event is flattened).
func dialFailureHint(errText string) string {
	for _, substr := range []string{"connection refused", "no such host", "i/o timeout", "dial tcp"} {
		if strings.Contains(errText, substr) {
			return " (is the model server up? its endpoint is set in providers.default.endpoint)"
		}
	}
	return ""
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

// RunChatSend is `quack chat send <id> "<msg>"`: a non-interactive turn on an existing chat, e.g. to
// answer a needs_input question. showEvents traces the pipeline to errOut. Returns the exit code.
func RunChatSend(ctx context.Context, out, errOut io.Writer, server, id, content string, attachPaths []string, showEvents, asJSON bool) int {
	c, err := NewClient(ctx, server)
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

// PrintPrompt is `quack -p`: create a chat and send the prompt via the same path as `chat send`.
// The chat id goes to errOut so stdout stays answer-only. Returns the exit code.
func PrintPrompt(ctx context.Context, out, errOut, events io.Writer, server, prompt string, attachPaths []string, asJSON bool) int {
	c, err := NewClient(ctx, server)
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
