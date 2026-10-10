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
	"strconv"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/schema"
)

// Client talks to a quack server's REST + SSE API. No per-request HTTP timeout on purpose:
// a research run streams for minutes, so the caller's context bounds request lifetime.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient resolves the server URL (override, then active registry, then localhost). A registered server
// with a stored OIDC session gets a Bearer token, refreshed if near expiry; ctx bounds only that refresh.
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

// notFoundErr wraps ErrNotFound with the server's own 404 message, so
// errors.Is still matches while nodeErrAs can recover the real reason.
type notFoundErr struct{ msg string }

func (e *notFoundErr) Error() string   { return e.msg }
func (e *notFoundErr) Is(t error) bool { return t == ErrNotFound }

// wrapNotFound turns a 404 response body into an error: ErrNotFound itself
// when the server gave no message, else a notFoundErr carrying it.
func wrapNotFound(msg string) error {
	if msg == "" {
		return ErrNotFound
	}
	return &notFoundErr{msg: msg}
}

// ListChats pages through every chat. statuses repeats as `?status=` - the
// archived/active scope, unrelated to a chat's own idle/running/failed status.
func (c *Client) ListChats(ctx context.Context, statuses []string) ([]schema.ChatSummary, error) {
	var all []schema.ChatSummary
	pageToken := ""
	for {
		q := url.Values{"limit": {"100"}}
		for _, s := range statuses {
			q.Add("status", s)
		}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var out schema.ChatList
		if err := c.getJSON(ctx, "/api/v1/chats?"+q.Encode(), &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPageToken == nil || *out.NextPageToken == "" {
			return all, nil
		}
		pageToken = *out.NextPageToken
	}
}

// UpdateChat renames and/or archives a chat; a nil field is left unchanged.
func (c *Client) UpdateChat(ctx context.Context, id string, title *string, archived *bool) (schema.ChatSummary, error) {
	var out schema.ChatSummary
	body, _ := json.Marshal(schema.UpdateChatBody{Title: title, Archived: archived})
	status, respBody, err := c.Request(ctx, http.MethodPatch, "/api/v1/chats/"+id, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	if status == http.StatusNotFound {
		return out, wrapNotFound(errBody(bytes.NewReader(respBody)))
	}
	if status >= 400 {
		return out, fmt.Errorf("PATCH .../chats/%s: %s", id, errStatus(status, respBody))
	}
	return out, json.Unmarshal(respBody, &out)
}

// ListRecordings returns every session with a ledger recording, unsorted.
// 404 (recording disabled) surfaces as ErrNotFound.
func (c *Client) ListRecordings(ctx context.Context) ([]schema.RecordingSummary, error) {
	var out schema.RecordingList
	if err := c.getJSON(ctx, "/api/v1/recordings", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ListDecisions fetches decision ledger entries via GET /api/v1/decisions. 404
// (recording disabled) surfaces as ErrNotFound.
func (c *Client) ListDecisions(ctx context.Context, f DecisionFilter, withState bool) (schema.DecisionList, error) {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if f.Point != "" {
		q.Set("point", f.Point)
	}
	for _, id := range f.Chats {
		q.Add("chat", id)
	}
	if withState {
		q.Set("with_state", "true")
	}
	var out schema.DecisionList
	err := c.getJSON(ctx, "/api/v1/decisions?"+q.Encode(), &out)
	return out, err
}

// GetVersion fetches the server's build version via GET /api/v1/config;
// "" if the server doesn't report one.
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var cfg schema.ClientConfig
	if err := c.getJSON(ctx, "/api/v1/config", &cfg); err != nil {
		return "", err
	}
	if cfg.Version == nil {
		return "", nil
	}
	return *cfg.Version, nil
}

// ListChatArtifacts returns every artifact name visible to a chat, each with
// its full revision history. 404 (unknown chat) surfaces as ErrNotFound.
func (c *Client) ListChatArtifacts(ctx context.Context, chatID string) ([]schema.ArtifactSummary, error) {
	var out schema.ArtifactList
	if err := c.getJSON(ctx, "/api/v1/chats/"+chatID+"/artifacts", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// FetchArtifact downloads one artifact revision's bytes - latest when
// revision is 0. 404 (unknown chat/artifact/revision) surfaces as ErrNotFound.
func (c *Client) FetchArtifact(ctx context.Context, chatID, artifactName string, revision int) ([]byte, error) {
	path := "/api/v1/chats/" + chatID + "/artifacts/" + url.PathEscape(artifactName)
	if revision > 0 {
		path += "?revision=" + strconv.Itoa(revision)
	}
	status, body, err := c.Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status >= 400 {
		return nil, fmt.Errorf("GET .../artifacts/%s: %s", artifactName, errStatus(status, body))
	}
	return body, nil
}

// ListMemories browses or (with q) searches memory. limit<=0 auto-pages the
// whole listing (like ListChats); q is a bounded top-K search, never paged.
func (c *Client) ListMemories(ctx context.Context, bucket, q, tier, sort string, limit int, includeInvalidated bool) (schema.MemoryList, error) {
	if q != "" || limit > 0 {
		var out schema.MemoryList
		err := c.getJSON(ctx, "/api/v1/memories?"+memoryQuery(bucket, q, tier, sort, limit, includeInvalidated, "").Encode(), &out)
		return out, err
	}
	// Starts non-nil: append onto a nil slice with nothing to append stays
	// nil, which would encode as `null` instead of `[]`.
	all := schema.MemoryList{Memories: []schema.Memory{}}
	pageToken := ""
	for {
		var page schema.MemoryList
		if err := c.getJSON(ctx, "/api/v1/memories?"+memoryQuery(bucket, "", tier, sort, 0, includeInvalidated, pageToken).Encode(), &page); err != nil {
			return all, err
		}
		all.Memories = append(all.Memories, page.Memories...)
		all.Total = page.Total
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			return all, nil
		}
		pageToken = *page.NextPageToken
	}
}

func memoryQuery(bucket, q, tier, sort string, limit int, includeInvalidated bool, pageToken string) url.Values {
	v := url.Values{}
	if bucket != "" {
		v.Set("bucket", bucket)
	}
	if q != "" {
		v.Set("q", q)
	}
	if tier != "" {
		v.Set("tier", tier)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	if includeInvalidated {
		v.Set("include_invalidated", "true")
	}
	if pageToken != "" {
		v.Set("page_token", pageToken)
	}
	return v
}

// GetMemory fetches one memory by id directly (GET /api/v1/memories/{id}) -
// no store-wide scan. 404 (unknown id) surfaces as ErrNotFound.
func (c *Client) GetMemory(ctx context.Context, id string) (schema.Memory, error) {
	var out schema.Memory
	err := c.getJSON(ctx, "/api/v1/memories/"+url.PathEscape(id), &out)
	return out, err
}

// ForgetMemory invalidates (soft-deletes) one memory. 404 (unknown id)
// surfaces as ErrNotFound.
func (c *Client) ForgetMemory(ctx context.Context, memoryID, reason string) error {
	return c.sendBody(ctx, http.MethodDelete, "/api/v1/memories/"+memoryID, schema.DeleteMemoryBody{Reason: &reason})
}

// SweepMemories runs the forgetting-rule sweep on demand. dedupe switches to the per-bucket similarity
// dedupe sweep; dryRun/apply control whether either writes or only reports.
func (c *Client) SweepMemories(ctx context.Context, dryRun, dedupe, apply bool) (schema.SweepMemoriesResult, error) {
	var out schema.SweepMemoriesResult
	err := c.postJSON(ctx, "/api/v1/memories/sweep", schema.SweepMemoriesBody{DryRun: &dryRun, Dedupe: &dedupe, Apply: &apply}, &out)
	return out, err
}

// RescopeMemories moves role:* memories with a resolvable GitHub-origin chat
// into their repo:* bucket. apply=false only tallies.
func (c *Client) RescopeMemories(ctx context.Context, apply bool) (schema.RescopeReport, error) {
	var out schema.RescopeReport
	err := c.postJSON(ctx, "/api/v1/memories/rescope", schema.RescopeMemoriesBody{Apply: &apply}, &out)
	return out, err
}

// GetMemoryStats fetches weekly recall precision/vote/recall counts plus a per-scope snapshot.
func (c *Client) GetMemoryStats(ctx context.Context, weeks int) (schema.MemoryStats, error) {
	var out schema.MemoryStats
	path := "/api/v1/memories/stats"
	if weeks > 0 {
		path += "?weeks=" + strconv.Itoa(weeks)
	}
	err := c.getJSON(ctx, path, &out)
	return out, err
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

// CancelRun cancels the chat's active run by response id (from its response_created SSE event);
// a stale or finished id 404s.
func (c *Client) CancelRun(ctx context.Context, chatID, responseID string) error {
	return c.sendBody(ctx, http.MethodPut, "/api/v1/chats/"+chatID+"/responses/"+responseID+"/status",
		schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
}

// CancelNode stops one non-terminal node (running, queued or paused); the rest of the DAG continues.
func (c *Client) CancelNode(ctx context.Context, chatID, nodeID string) error {
	return c.send(ctx, http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/stop")
}

// PauseNode suspends a running node at its next turn boundary. Resume is a fresh re-run, not a frozen
// checkpoint: ADK's static graph needs the node to return to unblock its dependents.
func (c *Client) PauseNode(ctx context.Context, chatID, nodeID string) error {
	return c.sendBody(ctx, http.MethodPut, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusUpdateBodyStatusPaused})
}

// ResumeNode resumes a paused node: a fresh re-run (like retry), reusing the
// rest of the plan's stored outputs. Only legal from `paused`.
func (c *Client) ResumeNode(ctx context.Context, chatID, nodeID string) error {
	return c.sendBody(ctx, http.MethodPut, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status",
		schema.NodeStatusUpdateBody{Status: schema.NodeStatusUpdateBodyStatusRunning})
}

// QueueNodeMessage queues a message for a running node, delivered at its next turn boundary.
// 404 if the node isn't currently running.
func (c *Client) QueueNodeMessage(ctx context.Context, chatID, nodeID, text string) (schema.QueuedMessage, error) {
	var out schema.QueuedMessage
	b, _ := json.Marshal(schema.QueueMessageBody{Message: text})
	status, respBody, err := c.Request(ctx, http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	if status == http.StatusNotFound {
		return out, wrapNotFound(errBody(bytes.NewReader(respBody)))
	}
	if status >= 400 {
		return out, httpFailure(http.MethodPost, ".../queue", status, respBody)
	}
	return out, json.Unmarshal(respBody, &out)
}

// EditQueuedMessage rewrites a not-yet-delivered queued message. Errors
// (surfaced as a 409) if it was already delivered.
func (c *Client) EditQueuedMessage(ctx context.Context, chatID, nodeID, messageID, text string) error {
	return c.sendBody(ctx, http.MethodPatch, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue/"+messageID, schema.QueueMessageBody{Message: text})
}

// RemoveQueuedMessage drops a not-yet-delivered queued message. Errors
// (surfaced as a 409) if it was already delivered.
func (c *Client) RemoveQueuedMessage(ctx context.Context, chatID, nodeID, messageID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue/"+messageID)
}

// EditNodeTask replaces a not-yet-started node's task text. Errors (surfaced
// as a 409) once the node has started - its prompt is then immutable.
func (c *Client) EditNodeTask(ctx context.Context, chatID, nodeID, task string) error {
	return c.sendBody(ctx, http.MethodPatch, "/api/v1/chats/"+chatID+"/nodes/"+nodeID, schema.EditNodeTaskBody{Task: task})
}

// sendBody issues a request with body JSON-encoded and discards the response;
// 404 → ErrNotFound, mirroring send.
func (c *Client) sendBody(ctx context.Context, method, path string, body any) error {
	b, _ := json.Marshal(body)
	status, respBody, err := c.Request(ctx, method, path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return wrapNotFound(errBody(bytes.NewReader(respBody)))
	}
	if status >= 400 {
		return httpFailure(method, path, status, respBody)
	}
	return nil
}

// RetryNode re-queues a finished node; it and everything downstream re-run, reusing all other nodes'
// stored outputs. guidance is optional and folded into the node's task.
func (c *Client) RetryNode(ctx context.Context, chatID, nodeID, guidance string) error {
	body := schema.NodeStatusUpdateBody{Status: schema.NodeStatusUpdateBodyStatusQueued}
	if guidance != "" {
		body.Guidance = &guidance
	}
	return c.sendBody(ctx, http.MethodPut, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status", body)
}

// errBody extracts a readable reason from an error body: the JSON "error" field (plus "allowed"
// statuses when present), else the raw text; "" when empty or unreadable.
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
		return fmt.Errorf("GET %s: %s", path, errStatus(status, body))
	}
	return json.Unmarshal(body, out)
}

// send issues a bodiless request and discards the response; 404 → ErrNotFound
// (wrapping the server's message, if any - see wrapNotFound).
func (c *Client) send(ctx context.Context, method, path string) error {
	status, body, err := c.Request(ctx, method, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return wrapNotFound(errBody(bytes.NewReader(body)))
	}
	if status >= 400 {
		return httpFailure(method, path, status, body)
	}
	return nil
}

// ConflictError is a 409: the server refused because the resource's state
// moved on. Current is the node status when the body carries one.
type ConflictError struct{ Msg, Current string }

func (e *ConflictError) Error() string { return e.Msg }

// httpFailure is the one place a >=400 response becomes an error, so every
// command renders a 409 the same way instead of echoing the raw exchange.
func httpFailure(method, path string, status int, body []byte) error {
	if status == http.StatusConflict {
		var te struct {
			Current string `json:"current"`
		}
		_ = json.Unmarshal(body, &te)
		return &ConflictError{Msg: errBody(bytes.NewReader(body)), Current: te.Current}
	}
	return fmt.Errorf("%s %s: %s", method, path, errStatus(status, body))
}

// errStatus renders a 4xx/5xx as "HTTP <status>: <server's reason>" using the
// server's schema.ErrorResponse body when present, else just the status.
func errStatus(status int, body []byte) string {
	if msg := errBody(bytes.NewReader(body)); msg != "" {
		return fmt.Sprintf("HTTP %d: %s", status, msg)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// readAll reads r best-effort - "" on any error, since it only ever feeds an
// already-failed response's body into errStatus for a nicer message.
func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 4096))
	return b
}

// FetchRecording downloads a chat's ledger recording bundle (ZIP) for `quack eval` and
// `quack experiment run`. ErrNotFound when the chat has no recording.
func (c *Client) FetchRecording(ctx context.Context, chatID string) ([]byte, error) {
	status, body, err := c.Request(ctx, http.MethodGet, "/api/v1/chats/"+chatID+"/recording", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status >= 400 {
		return nil, fmt.Errorf("GET .../recording: %s", errStatus(status, body))
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

// Request performs an arbitrary REST call and returns the status and body. path may omit the leading
// "/"; a non-nil body is sent as JSON.
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
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// RunAPI is `quack api`, a raw REST passthrough (like `gh api`): it writes the response body to out
// and returns an error on 4xx/5xx so the command exits non-zero.
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
	// ID is the SSE `id:` field (the durable-log seq a reconnect resumes past via Last-Event-ID);
	// only subscribeSSE events carry one.
	ID string
}

// Reconnect backoff for subscribeSSE: capped so a dead server isn't hammered, bounded so a
// permanently unreachable one eventually surfaces as an error.
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

// Subscribe attaches to a chat's live or just-finished run via the GET stream endpoint: the hub
// replays events so far, then tails live. Same channel contract as Stream.
func (c *Client) Subscribe(ctx context.Context, chatID string) <-chan SSEEvent {
	return c.streamChan(ctx, func(onEvent func(SSEEvent) error) error {
		return c.subscribeSSE(ctx, chatID, onEvent)
	})
}

// streamChan pumps an SSE-producing call's events to a channel, closing on completion or cancel
// and surfacing a transport error as a final error event.
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

// subscribeSSE dispatches the chat stream's events to onEvent until a `done` event or ctx cancel. A drop
// before `done` is retried with backoff, resuming via Last-Event-ID so `chat show -f` survives breaks.
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe: %s", errStatus(resp.StatusCode, readAll(resp.Body)))
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send message: %s", errStatus(resp.StatusCode, readAll(resp.Body)))
	}
	return parseSSE(resp.Body, onEvent)
}

// SendMessageWithFiles posts content plus attachments as multipart/form-data and streams the SSE response.
// Each file's Content-Type comes from its extension so a media-capable node gets the right MIME.
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
			_ = f.Close()
			return err
		}
		if _, err := io.Copy(fw, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("attach %s: %w", p, err)
		}
		_ = f.Close()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send message: %s", errStatus(resp.StatusCode, readAll(resp.Body)))
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: %s", path, errStatus(resp.StatusCode, readAll(resp.Body)))
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

// parseSSE dispatches each SSE event in r to onEvent: `event:`/`id:`/`data:` lines, blank-line separated;
// multiple data lines join with newlines (per spec) and `:` comment lines are ignored.
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
