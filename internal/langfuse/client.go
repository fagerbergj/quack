// Package langfuse is a small client over Langfuse's prompt-management public API
// (https://api.reference.langfuse.com), used as a prompts: store in P2 of #1421.
package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/httpx"
)

// Prompt is a resolved Langfuse prompt version. Only type "text" is supported;
// a chat-type prompt is returned as an error naming it (see GetPrompt).
type Prompt struct {
	Name          string
	Version       int
	Body          string
	Config        map[string]any
	Labels        []string
	Tags          []string
	CommitMessage string
}

// CreatePromptRequest is the body for CreatePrompt.
type CreatePromptRequest struct {
	Name          string
	Body          string
	Config        map[string]any
	Labels        []string
	Tags          []string
	CommitMessage string
}

// Client talks to the Langfuse public API with Basic auth (public_key:secret_key).
type Client struct {
	baseURL    string
	publicKey  string
	secretKey  string
	pinLabel   string
	httpClient *http.Client
}

// New builds a Client. opts can override the default resilient http.Client and set
// the label Resolve pins to (production, say); empty means Resolve only tries latest.
func New(baseURL, publicKey, secretKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		publicKey: publicKey,
		secretKey: secretKey,
		httpClient: &http.Client{
			Transport: httpx.NewTransport(nil),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (tests use this to point at an httptest.Server).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithPinLabel sets the label Resolve tries before falling back to latest.
func WithPinLabel(label string) Option {
	return func(c *Client) { c.pinLabel = label }
}

// APIError is a non-2xx response from Langfuse; the body is truncated for logging.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("langfuse: status %d: %s", e.Status, e.Body)
}

// permanentError marks a non-HTTP failure (bad response shape, bad call arguments)
// as not worth retrying or falling back identically on - see IsTransient.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func markPermanent(err error) error { return &permanentError{err: err} }

// IsTransient reports whether err is a 429/5xx response or a network-level failure -
// the kind of failure a caller should fall back to static content for. 4xx (other than
// 429), malformed responses, and a caller mistake (e.g. bad GetPromptOpts) are permanent.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	var perm *permanentError
	if errors.As(err, &perm) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Anything else reaching here is a transport/network failure (dial, timeout, reset).
	return true
}

// IsAuthError reports whether err is a 401/403 from Langfuse, i.e. a misconfigured
// key rather than an outage - callers should log this distinctly, not silently fall back.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
	}
	return false
}

const maxErrBody = 1000
const maxOKBody = 1 << 20 // 1MB: generous ceiling for a prompt body, guards against a runaway response

func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody any) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, markPermanent(fmt.Errorf("langfuse: encode request: %w", err))
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, markPermanent(fmt.Errorf("langfuse: build request: %w", err))
	}
	req.SetBasicAuth(c.publicKey, c.secretKey)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("langfuse: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// GetPromptOpts selects which version GetPrompt resolves; Label and Version are mutually exclusive.
type GetPromptOpts struct {
	Label   string
	Version int
}

// promptWire is the Langfuse v2 prompt response shape. Type "chat" carries prompt
// as an array of messages instead of a string; that shape is rejected in GetPrompt.
type promptWire struct {
	Name          string          `json:"name"`
	Version       int             `json:"version"`
	Type          string          `json:"type"`
	Prompt        json.RawMessage `json:"prompt"`
	Config        map[string]any  `json:"config"`
	Labels        []string        `json:"labels"`
	Tags          []string        `json:"tags"`
	CommitMessage string          `json:"commitMessage"`
}

// GetPrompt fetches a prompt by name via GET /api/public/v2/prompts/{name}.
// Returns (_, false, nil) on 404. A chat-type prompt is an error naming it - only text is supported.
func (c *Client) GetPrompt(ctx context.Context, name string, opts GetPromptOpts) (Prompt, bool, error) {
	if opts.Label != "" && opts.Version != 0 {
		return Prompt{}, false, markPermanent(fmt.Errorf("langfuse: GetPrompt %q: label and version are mutually exclusive", name))
	}
	q := url.Values{}
	if opts.Label != "" {
		q.Set("label", opts.Label)
	}
	if opts.Version != 0 {
		q.Set("version", strconv.Itoa(opts.Version))
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/public/v2/prompts/"+url.PathEscape(name), q, nil)
	if err != nil {
		return Prompt{}, false, fmt.Errorf("langfuse: get %q: %w", name, err)
	}
	defer drain(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return Prompt{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Prompt{}, false, fmt.Errorf("langfuse: get %q: %w", name, newAPIError(resp))
	}

	var w promptWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOKBody)).Decode(&w); err != nil {
		return Prompt{}, false, markPermanent(fmt.Errorf("langfuse: decode prompt %q: %w", name, err))
	}
	if w.Type != "" && w.Type != "text" {
		return Prompt{}, false, markPermanent(fmt.Errorf("langfuse: prompt %q is type %q, only text prompts are supported", name, w.Type))
	}
	var body string
	if err := json.Unmarshal(w.Prompt, &body); err != nil {
		return Prompt{}, false, markPermanent(fmt.Errorf("langfuse: prompt %q is not a text prompt", name))
	}
	return Prompt{
		Name:          w.Name,
		Version:       w.Version,
		Body:          body,
		Config:        w.Config,
		Labels:        w.Labels,
		Tags:          w.Tags,
		CommitMessage: w.CommitMessage,
	}, true, nil
}

// CreatePrompt creates a new text prompt version via POST /api/public/v2/prompts.
func (c *Client) CreatePrompt(ctx context.Context, req CreatePromptRequest) (Prompt, error) {
	wireReq := map[string]any{
		"type":          "text",
		"name":          req.Name,
		"prompt":        req.Body,
		"config":        req.Config,
		"labels":        nonNil(req.Labels),
		"tags":          nonNil(req.Tags),
		"commitMessage": req.CommitMessage,
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/public/v2/prompts", nil, wireReq)
	if err != nil {
		return Prompt{}, fmt.Errorf("langfuse: create %q: %w", req.Name, err)
	}
	defer drain(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Prompt{}, fmt.Errorf("langfuse: create %q: %w", req.Name, newAPIError(resp))
	}
	var w promptWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOKBody)).Decode(&w); err != nil {
		return Prompt{}, markPermanent(fmt.Errorf("langfuse: decode created prompt %q: %w", req.Name, err))
	}
	var body string
	_ = json.Unmarshal(w.Prompt, &body)
	return Prompt{
		Name:          w.Name,
		Version:       w.Version,
		Body:          body,
		Config:        w.Config,
		Labels:        w.Labels,
		Tags:          w.Tags,
		CommitMessage: w.CommitMessage,
	}, nil
}

// nonNil turns a nil slice into an empty one - Langfuse's API rejects a JSON
// null for labels/tags ("expected array, received null").
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}

// drain reads a response body to completion before closing, so the transport
// can reuse the connection (matches internal/httpx's own retry-path handling).
func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxOKBody))
	_ = body.Close()
}
