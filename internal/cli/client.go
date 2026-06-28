package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fagerbergj/quack/internal/schema"
)

// Client talks to a quack server's REST + SSE API. The HTTP client has no
// per-request timeout on purpose: a research run streams for minutes — request
// lifetime is bounded by the caller's context instead.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient resolves the server URL (override → active registry → localhost) and
// returns a ready client. override is the --server flag value ("" to use the
// active server from ~/.quack/servers.yaml).
func NewClient(override string) (*Client, error) {
	cc, err := LoadClient()
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL: strings.TrimRight(cc.ActiveURL(override), "/"),
		HTTP:    &http.Client{},
	}, nil
}

// ErrNotFound is returned by client calls when the server responds 404.
var ErrNotFound = errors.New("not found")

// ListChats returns all chats (server orders most-recently-updated first).
func (c *Client) ListChats(ctx context.Context) ([]schema.ChatSummary, error) {
	var out schema.ChatList
	if err := c.getJSON(ctx, "/api/v1/chats", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// GetChat returns a chat with its turns.
func (c *Client) GetChat(ctx context.Context, id string) (schema.ChatDetail, error) {
	var out schema.ChatDetail
	err := c.getJSON(ctx, "/api/v1/chats/"+id, &out)
	return out, err
}

// DeleteChat deletes a chat.
func (c *Client) DeleteChat(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/chats/"+id)
}

// CancelRun cancels the active orchestrator run for a chat (no-op if none).
func (c *Client) CancelRun(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/chats/"+id+"/stream")
}

// getJSON GETs path and decodes a JSON response into out; 404 → ErrNotFound.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	status, body, err := c.Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", path, status)
	}
	return json.Unmarshal(body, out)
}

// send issues a bodiless request and discards the response; 404 → ErrNotFound.
func (c *Client) send(ctx context.Context, method, path string) error {
	status, _, err := c.Request(ctx, method, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 400 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, status)
	}
	return nil
}

// CreateChat opens a new chat and returns its id. systemPrompt may be "".
func (c *Client) CreateChat(ctx context.Context, systemPrompt string) (string, error) {
	in := schema.CreateChatBody{}
	if systemPrompt != "" {
		in.SystemPrompt = &systemPrompt
	}
	var out schema.ChatSummary
	if err := c.postJSON(ctx, "/api/v1/chats", in, &out); err != nil {
		return "", err
	}
	return out.Id, nil
}

// Request performs an arbitrary REST call and returns the status code and the
// response body. path may be absolute ("/health") or root-relative ("health");
// body is nil for none (Content-Type defaults to JSON when a body is sent).
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader) (int, []byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), c.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, c.reachErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// RunAPI is `quack api`: a raw REST passthrough (à la `gh api`). It writes the
// response body to out and returns a non-nil error on a 4xx/5xx so the command
// exits non-zero. body is the request body (nil for none).
func RunAPI(ctx context.Context, out io.Writer, server, method, path string, body io.Reader) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	status, respBody, err := c.Request(ctx, method, path, body)
	if err != nil {
		return err
	}
	_, _ = out.Write(respBody)
	if n := len(respBody); n == 0 || respBody[n-1] != '\n' {
		fmt.Fprintln(out) // tidy terminal output; harmless for pipes
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

// SSEEvent is one decoded server-sent event: a name and its raw JSON data.
type SSEEvent struct {
	Name string
	Data json.RawMessage
}

// SendMessage posts content to a chat and calls onEvent for each SSE event until
// the stream ends. onEvent returning an error stops reading early.
func (c *Client) SendMessage(ctx context.Context, chatID, content string, onEvent func(SSEEvent) error) error {
	body, _ := json.Marshal(schema.SendMessageBody{Content: content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/chats/"+chatID+"/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.reachErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send message: server returned %s", resp.Status)
	}
	return parseSSE(resp.Body, onEvent)
}

// postJSON POSTs v as JSON to path and decodes the JSON response into out (nil to
// ignore the body).
func (c *Client) postJSON(ctx context.Context, path string, v, out any) error {
	body, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.reachErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: server returned %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// reachErr wraps a transport error with the actionable next step.
func (c *Client) reachErr(err error) error {
	return fmt.Errorf("couldn't reach quack server at %s: %w\n(is `quack server run` up? check with `quack server list`)", c.BaseURL, err)
}

// parseSSE reads an SSE body and dispatches each event to onEvent. Framing:
// `event:`/`data:` lines, events separated by a blank line; multiple data lines
// join with newlines (per the SSE spec); `:` comment lines are ignored.
func parseSSE(r io.Reader, onEvent func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a tool_result event can be large
	var name string
	var data strings.Builder
	flush := func() error {
		if name == "" && data.Len() == 0 {
			return nil
		}
		ev := SSEEvent{Name: name, Data: json.RawMessage(data.String())}
		name, data = "", strings.Builder{}
		return onEvent(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush() // a final event may not be terminated by a blank line
}

// PrintPrompt runs one prompt against the server and streams the assistant's
// reply to out (no TUI) — this is `quack -p`. The visible answer is agent_token
// text with no node_id (the top-level orchestrator reply); node-scoped tokens are
// intermediate research output and are not printed. stdout stays clean for pipes;
// the caller routes errors to stderr.
func PrintPrompt(ctx context.Context, out io.Writer, server, prompt string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		return err
	}
	var streamErr error
	printed := false
	err = c.SendMessage(ctx, chatID, prompt, func(ev SSEEvent) error {
		switch ev.Name {
		case "agent_token":
			var d struct {
				NodeID string `json:"node_id"`
				Text   string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.NodeID == "" && d.Text != "" {
				fmt.Fprint(out, d.Text)
				printed = true
			}
		case "error":
			var d struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			streamErr = fmt.Errorf("server error: %s", d.Error)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if streamErr != nil {
		return streamErr
	}
	if printed {
		fmt.Fprintln(out) // newline after the streamed answer
	}
	return nil
}
