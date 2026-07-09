package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
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

// CancelRun cancels the chat's active run by response id (the id surfaced in
// the run's opening response_created SSE event) — only legal while that
// response is the active run; a stale/finished id 404s.
func (c *Client) CancelRun(ctx context.Context, chatID, responseID string) error {
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/responses/"+responseID+"/status",
		schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
}

// CancelNode stops one running node of a chat's active run; the rest of the DAG
// continues (continue-but-warn). No-op if no such node is active.
func (c *Client) CancelNode(ctx context.Context, chatID, nodeID string) error {
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusCancelled})
}

// SteerNode interrupts one running node and re-runs it with new guidance against
// its same session (prior tool calls/results retained). No-op if no such node is
// active. The re-run streams over the chat's existing SSE connection.
func (c *Client) SteerNode(ctx context.Context, chatID, nodeID, guidance string) error {
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning, Guidance: &guidance})
}

// putStatus PUTs body (a *StatusUpdateBody schema type) to path — the shared
// shape of the node/response status-transition endpoints.
func (c *Client) putStatus(ctx context.Context, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.reachErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: server returned %s", path, resp.Status)
	}
	return nil
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

// Stream posts content to a chat and returns a channel of SSE events for the TUI
// pump. The channel closes when the run ends (done) or ctx is cancelled. A
// transport/HTTP error is delivered as a final SSEEvent{Name:"error"} so the UI
// renders it uniformly rather than the caller having to handle two error paths.
func (c *Client) Stream(ctx context.Context, chatID, content string) <-chan SSEEvent {
	return c.streamChan(ctx, func(onEvent func(SSEEvent) error) error {
		return c.SendMessage(ctx, chatID, content, onEvent)
	})
}

// Subscribe attaches to a chat's live (or just-finished) run via the standalone
// GET stream endpoint — for resuming a run started elsewhere, or by this client
// before a reconnect. The hub replays the events so far, then tails live. Same
// channel contract as Stream.
func (c *Client) Subscribe(ctx context.Context, chatID string) <-chan SSEEvent {
	return c.streamChan(ctx, func(onEvent func(SSEEvent) error) error {
		return c.subscribeSSE(ctx, chatID, onEvent)
	})
}

// streamChan runs an SSE-producing call on a goroutine and pumps its events to a
// channel, closing on completion/cancel and surfacing a transport error as a
// final error event. Shared by Stream (POST) and Subscribe (GET).
func (c *Client) streamChan(ctx context.Context, run func(onEvent func(SSEEvent) error) error) <-chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	go func() {
		defer close(ch)
		err := run(func(ev SSEEvent) error {
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && ctx.Err() == nil {
			data, _ := json.Marshal(map[string]string{"error": err.Error()})
			select {
			case ch <- SSEEvent{Name: "error", Data: data}:
			case <-ctx.Done():
			}
		}
	}()
	return ch
}

// subscribeSSE GETs the chat's stream endpoint and dispatches each SSE event to
// onEvent until the stream ends.
func (c *Client) subscribeSSE(ctx context.Context, chatID string, onEvent func(SSEEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/chats/"+chatID+"/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.reachErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe: server returned %s", resp.Status)
	}
	return parseSSE(resp.Body, onEvent)
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

// SendMessageWithFiles posts content plus file attachments (image/audio) as
// multipart/form-data (field "content" + repeated "files") and streams the SSE
// response. The per-file Content-Type is inferred from the extension so the
// server threads the right MIME to a media-capable node. Used by `-p --attach`.
func (c *Client) SendMessageWithFiles(ctx context.Context, chatID, content string, filePaths []string, onEvent func(SSEEvent) error) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("content", content); err != nil {
		return err
	}
	for _, p := range filePaths {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("attach %s: %w", p, err)
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, filepath.Base(p)))
		ct := mime.TypeByExtension(filepath.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		fw, err := mw.CreatePart(h)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(fw, f); err != nil {
			f.Close()
			return fmt.Errorf("attach %s: %w", p, err)
		}
		f.Close()
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/chats/"+chatID+"/responses", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
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

// PrintPrompt (`quack -p`) and RunChatSend (`quack chat send`) live in send.go,
// built on SendMessage/SendMessageWithFiles below — they share one
// classify-the-outcome path (streamState) so their pause/failure semantics agree.
