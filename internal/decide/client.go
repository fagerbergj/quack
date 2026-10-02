// Package decide runs handlers at named intercept points and applies a per-point
// policy to their calibrated probabilities. The one handler kind is a System One
// model (POST /v1/systemone: Clef, Kev, Jev). A failure is "no decision", never a block.
package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/httpx"
)

// charsPerToken is a deliberately low estimate so the pre-call guard only skips
// inputs that are certainly over the cap; the server's 413 stays authoritative.
const charsPerToken = 4

// ErrTooLarge: the input is over the handler's max_input_tokens (pre-call guard or HTTP 413).
var ErrTooLarge = errors.New("decide: input over the handler's token cap")

// Question is one systemone question. Criteria: noul {true?, false?},
// choice {option: description|null}, score [level, ...] lowest first.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Handler answers a point's questions with per-option probabilities.
type Handler interface {
	Ask(ctx context.Context, state any, questions map[string]Question) (Reply, error)
}

// newHandler builds a handler of h.Kind; config validation admits only systemone.
func newHandler(h config.DecisionHandler) Handler { return NewClient(h) }

// Client is the systemone handler: one /v1/systemone endpoint.
type Client struct {
	url            string
	model          string
	timeout        time.Duration
	maxInputTokens int
	http           *http.Client
}

// NewClient retries once on dial errors, 429 and 5xx (503 is the server's
// per-batch OOM); 413/422 are never retried.
func NewClient(p config.DecisionHandler) *Client {
	return &Client{
		url:            strings.TrimRight(p.URL, "/") + "/v1/systemone",
		model:          p.Model,
		timeout:        p.Timeout,
		maxInputTokens: p.MaxInputTokens,
		http:           &http.Client{Transport: httpx.NewTransport(nil, httpx.WithMaxAttempts(2), httpx.WithBaseDelay(100*time.Millisecond))},
	}
}

// Reply is one call's per-question option probabilities (a noul's are "true"/"false").
type Reply struct {
	Answers      map[string]map[string]float64
	RequestBytes int
	InputTokens  int
	ServerMS     float64
}

type wireAnswer struct {
	Noul          *float64           `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type wireResponse struct {
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
	LatencyMS float64 `json:"latency_ms"`
}

// Ask sends one request; RequestBytes is set even when err != nil.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (Reply, error) {
	body, err := json.Marshal(map[string]any{"model": c.model, "state": state, "questions": questions})
	if err != nil {
		return Reply{}, fmt.Errorf("decide: encode request: %w", err)
	}
	reply := Reply{RequestBytes: len(body)}
	if c.maxInputTokens > 0 && len(body)/charsPerToken > c.maxInputTokens {
		return reply, fmt.Errorf("%w: ~%d tokens, cap %d", ErrTooLarge, len(body)/charsPerToken, c.maxInputTokens)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(httpx.WithIdempotent(ctx), http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return reply, fmt.Errorf("decide: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return reply, fmt.Errorf("decide: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			return reply, fmt.Errorf("%w: %s", ErrTooLarge, detail)
		}
		if resp.StatusCode == http.StatusUnprocessableEntity {
			// quack sent a schema the server rejects: a bug here, not an outage.
			slog.Warn("decision request rejected", "component", "decide", "url", c.url, "detail", string(detail))
		}
		return reply, fmt.Errorf("decide: %s: status %d: %s", c.url, resp.StatusCode, detail)
	}
	var wr wireResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wr); err != nil {
		return reply, fmt.Errorf("decide: decode response: %w", err)
	}
	reply.InputTokens, reply.ServerMS = wr.Usage.InputTokens, wr.LatencyMS
	reply.Answers = make(map[string]map[string]float64, len(wr.Answers))
	for id, a := range wr.Answers {
		if a.Noul != nil {
			reply.Answers[id] = map[string]float64{"true": *a.Noul, "false": 1 - *a.Noul}
		} else if len(a.Probabilities) > 0 {
			reply.Answers[id] = a.Probabilities
		}
	}
	return reply, nil
}
