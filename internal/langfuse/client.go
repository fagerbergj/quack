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
	httpClient *http.Client
}

// New builds a Client. opts can override the default resilient http.Client.
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

// APIError is a non-2xx response from Langfuse; the body is truncated for logging.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("langfuse: status %d: %s", e.Status, e.Body)
}

// IsTransient reports whether err is a 5xx response or a network-level failure,
// i.e. the kind of failure a caller should fall back to static content for.
func IsTransient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	// Any non-API error reaching here is a network/transport failure.
	return err != nil
}

const maxErrBody = 1000

func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody any) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("langfuse: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("langfuse: build request: %w", err)
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
	q := url.Values{}
	if opts.Label != "" {
		q.Set("label", opts.Label)
	}
	if opts.Version != 0 {
		q.Set("version", strconv.Itoa(opts.Version))
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/public/v2/prompts/"+url.PathEscape(name), q, nil)
	if err != nil {
		return Prompt{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Prompt{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Prompt{}, false, newAPIError(resp)
	}

	var w promptWire
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return Prompt{}, false, fmt.Errorf("langfuse: decode prompt %q: %w", name, err)
	}
	if w.Type != "" && w.Type != "text" {
		return Prompt{}, false, fmt.Errorf("langfuse: prompt %q is type %q, only text prompts are supported", name, w.Type)
	}
	var body string
	if err := json.Unmarshal(w.Prompt, &body); err != nil {
		return Prompt{}, false, fmt.Errorf("langfuse: prompt %q is not a text prompt", name)
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
		"labels":        req.Labels,
		"tags":          req.Tags,
		"commitMessage": req.CommitMessage,
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/public/v2/prompts", nil, wireReq)
	if err != nil {
		return Prompt{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Prompt{}, newAPIError(resp)
	}
	var w promptWire
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return Prompt{}, fmt.Errorf("langfuse: decode created prompt %q: %w", req.Name, err)
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

func newAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}
