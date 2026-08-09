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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/schema"
)

// Client talks to a quack server's REST + SSE API. The HTTP client has no
// per-request timeout on purpose: a research run streams for minutes - request
// lifetime is bounded by the caller's context instead.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient resolves the server URL (override → active registry → localhost)
// and returns a ready client. override is the --server flag value ("" to use
// the active server from ~/.quack/servers.yaml). If the resolved URL matches
// a registered server that has a stored OIDC session (`quack server login`),
// every request attaches its access token as a Bearer credential - refreshed
// first if it's at or near expiry. ctx bounds that refresh call only; it does
// not outlive NewClient.
func NewClient(ctx context.Context, override string) (*Client, error) {
	cc, err := LoadClient()
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(cc.ActiveURL(override), "/")
	httpClient := &http.Client{Transport: httpx.NewTransport(nil)}
	if name, ref, ok := cc.findByURL(url); ok && ref.Auth != nil {
		token, err := ensureFreshToken(ctx, cc, name, ref)
		if err != nil {
			return nil, err
		}
		if token != "" {
			httpClient.Transport = &bearerTransport{token: token, base: httpClient.Transport}
		}
	}
	return &Client{
		BaseURL: url,
		HTTP:    httpClient,
	}, nil
}

// ErrNotFound is returned by client calls when the server responds 404.
var ErrNotFound = errors.New("not found")

// ListChats returns all chats (server orders most-recently-updated first),
// paging through the server's paginated endpoint at its max page size. The
// page token is opaque - passed back exactly as the server returned it,
// never parsed or constructed.
func (c *Client) ListChats(ctx context.Context) ([]schema.ChatSummary, error) {
	var all []schema.ChatSummary
	pageToken := ""
	for {
		path := "/api/v1/chats?limit=100"
		if pageToken != "" {
			path += "&page_token=" + url.QueryEscape(pageToken)
		}
		var out schema.ChatList
		if err := c.getJSON(ctx, path, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPageToken == nil || *out.NextPageToken == "" {
			return all, nil
		}
		pageToken = *out.NextPageToken
	}
}

// ListRecordings returns every session the replay ledger has an entry for
// (server orders however LedgerStore.List does; today that's directory
// order, unsorted). 404 (recording disabled) surfaces as ErrNotFound.
func (c *Client) ListRecordings(ctx context.Context) ([]schema.RecordingSummary, error) {
	var out schema.RecordingList
	if err := c.getJSON(ctx, "/api/v1/recordings", &out); err != nil {
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
// the run's opening response_created SSE event) - only legal while that
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

// PauseNode suspends one running node at its next turn boundary, keeping its
// accumulated work (resumable). No-op if no such node is active.
//
// ponytail note: pause is a real, working feature (not a stub), but resume is
// a FRESH re-run (like retry), not a literal frozen-thread checkpoint - ADK
// v2's static workflow graph needs the node to return to unblock its
// dependents, so there is no way to freeze it mid-tool-call the way an
// ask_user HITL pause does. See dag.Executor.PauseNode's own note.
func (c *Client) PauseNode(ctx context.Context, chatID, nodeID string) error {
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusPaused})
}

// ResumeNode resumes a paused node: a fresh re-run (like retry), reusing the
// rest of the plan's stored outputs. Only legal from `paused`.
func (c *Client) ResumeNode(ctx context.Context, chatID, nodeID string) error {
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
}

// QueueNodeMessage appends a message to a running node's queue, delivered at
// its next turn boundary (never mid-turn) - replaces the old interrupt-based
// SteerNode. Returns the created queued message (its id, for later editing or
// removal). 404 if the node isn't currently running.
func (c *Client) QueueNodeMessage(ctx context.Context, chatID, nodeID, text string) (schema.QueuedMessage, error) {
	var out schema.QueuedMessage
	b, _ := json.Marshal(schema.QueueMessageBody{Message: text})
	status, respBody, err := c.Request(ctx, http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	if status == http.StatusNotFound {
		return out, ErrNotFound
	}
	if status >= 400 {
		return out, fmt.Errorf("POST .../queue: %s", errBody(bytes.NewReader(respBody)))
	}
	return out, json.Unmarshal(respBody, &out)
}

// EditQueuedMessage rewrites a not-yet-delivered queued message. Errors
// (surfaced as a 409) if it was already delivered.
func (c *Client) EditQueuedMessage(ctx context.Context, chatID, nodeID, messageID, text string) error {
	b, _ := json.Marshal(schema.QueueMessageBody{Message: text})
	return c.sendBody(ctx, http.MethodPatch, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue/"+messageID, b)
}

// RemoveQueuedMessage drops a not-yet-delivered queued message. Errors
// (surfaced as a 409) if it was already delivered.
func (c *Client) RemoveQueuedMessage(ctx context.Context, chatID, nodeID, messageID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue/"+messageID)
}

// EditNodeTask replaces a not-yet-started node's task text. Errors (surfaced
// as a 409) once the node has started - its prompt is then immutable.
func (c *Client) EditNodeTask(ctx context.Context, chatID, nodeID, task string) error {
	b, _ := json.Marshal(schema.EditNodeTaskBody{Task: task})
	return c.sendBody(ctx, http.MethodPatch, "/api/v1/chats/"+chatID+"/nodes/"+nodeID, b)
}

// sendBody issues a request with a JSON body and discards the response;
// 404 → ErrNotFound, mirroring send.
func (c *Client) sendBody(ctx context.Context, method, path string, body []byte) error {
	status, respBody, err := c.Request(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 400 {
		if msg := errBody(bytes.NewReader(respBody)); msg != "" {
			return fmt.Errorf("%s %s: %s", method, path, msg)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, status)
	}
	return nil
}

// RetryNode re-queues a finished node (done, failed, or cancelled) - it and
// every node downstream re-run, reusing the stored outputs of all other nodes.
// guidance is optional and folded into the node's task.
func (c *Client) RetryNode(ctx context.Context, chatID, nodeID, guidance string) error {
	body := schema.NodeStatusUpdateBody{Status: schema.NodeStatusQueued}
	if guidance != "" {
		body.Guidance = &guidance
	}
	return c.putStatus(ctx, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status", body)
}

// putStatus PUTs body (a *StatusUpdateBody schema type) to path - the shared
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
		// Surface the server's reason (e.g. a 409 TransitionError names the
		// allowed target statuses) instead of a bare status line.
		if msg := errBody(resp.Body); msg != "" {
			return fmt.Errorf("PUT %s: %s: %s", path, resp.Status, msg)
		}
		return fmt.Errorf("PUT %s: server returned %s", path, resp.Status)
	}
	return nil
}

// errBody extracts a human-readable reason from an error response body: the
// JSON "error" field (plus "allowed" statuses when present, the TransitionError
// shape), else the raw body text. "" when the body is empty/unreadable.
func errBody(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var te struct {
		Error   string   `json:"error"`
		Allowed []string `json:"allowed"`
	}
	if json.Unmarshal(raw, &te) == nil && te.Error != "" {
		if len(te.Allowed) > 0 {
			return fmt.Sprintf("%s (allowed: %s)", te.Error, strings.Join(te.Allowed, ", "))
		}
		return te.Error
	}
	return string(bytes.TrimSpace(raw))
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

// FetchRecording downloads a chat's replay-ledger recording bundle (ZIP) -
// the fetch endpoint GET /api/v1/chats/{chat_id}/recording (#601). `quack
// replay <chat-id>` (#605) is the one caller today: it resolves a chat id
// into a local bundle file this way before driving a replayed run.
// ErrNotFound when the chat has no recording (never recorded, GC'd by
// retention, or recording disabled).
func (c *Client) FetchRecording(ctx context.Context, chatID string) ([]byte, error) {
	status, body, err := c.Request(ctx, http.MethodGet, "/api/v1/chats/"+chatID+"/recording", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status >= 400 {
		return nil, fmt.Errorf("GET .../recording: HTTP %d", status)
	}
	return body, nil
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
	c, err := NewClient(ctx, server)
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
	// ID is the SSE `id:` field, if any - the durable-log seq a reconnecting
	// subscriber resumes past via Last-Event-ID (set by subscribeSSE events
	// only; the POST send path doesn't carry ids).
	ID string
}

// Reconnect tuning for subscribeSSE's dropped-connection retry: capped
// exponential backoff so a dead server/proxy isn't hammered, bounded so a
// permanently unreachable server eventually surfaces as an error instead of
// retrying forever.
const (
	maxSSEReconnectAttempts = 6
	sseReconnectBaseDelay   = time.Second
	sseReconnectMaxDelay    = 15 * time.Second
)

// sseReconnectDelay is a var (not a plain func) so tests can shrink it -
// production behavior is the capped exponential backoff below.
var sseReconnectDelay = func(attempt int) time.Duration {
	d := sseReconnectBaseDelay << attempt
	if d <= 0 || d > sseReconnectMaxDelay { // overflow or past the cap
		return sseReconnectMaxDelay
	}
	return d
}

// Subscribe attaches to a chat's live (or just-finished) run via the standalone
// GET stream endpoint - for resuming a run started elsewhere, or by this client
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
// onEvent until the stream ends normally (a `done` event was seen) or ctx is
// cancelled. A connection dropped mid-run - no `done` seen, whether the body
// just closed or the request itself failed - is retried with capped
// exponential backoff, resuming past the last event actually delivered via
// Last-Event-ID (the server's durable event log, M8), so `chat show -f`
// recovers from a transient break without the caller doing anything.
func (c *Client) subscribeSSE(ctx context.Context, chatID string, onEvent func(SSEEvent) error) error {
	var lastID string
	var lastErr error
	for attempt := 0; attempt <= maxSSEReconnectAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(sseReconnectDelay(attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		sawDone := false
		err := c.subscribeSSEOnce(ctx, chatID, lastID, func(ev SSEEvent) error {
			if ev.ID != "" {
				lastID = ev.ID
			}
			if ev.Name == "done" {
				sawDone = true
			}
			return onEvent(ev)
		})
		if sawDone || ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("subscribe: lost connection to the server after %d attempts", maxSSEReconnectAttempts+1)
}

// subscribeSSEOnce is a single GET of the chat's stream endpoint, resuming
// past lastID via Last-Event-ID when reconnecting.
func (c *Client) subscribeSSEOnce(ctx context.Context, chatID, lastID string, onEvent func(SSEEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/chats/"+chatID+"/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
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
// `event:`/`id:`/`data:` lines, events separated by a blank line; multiple
// data lines join with newlines (per the SSE spec); `:` comment lines are
// ignored.
func parseSSE(r io.Reader, onEvent func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a tool_result event can be large
	var name, id string
	var data strings.Builder
	flush := func() error {
		if name == "" && data.Len() == 0 {
			return nil
		}
		ev := SSEEvent{Name: name, Data: json.RawMessage(data.String()), ID: id}
		name, id, data = "", "", strings.Builder{}
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
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(line[len("id:"):])
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
// built on SendMessage/SendMessageWithFiles below - they share one
// classify-the-outcome path (streamState) so their pause/failure semantics agree.
