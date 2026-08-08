// Package httpx provides quack's shared resilient http.RoundTripper: a
// method-aware retry policy that composes with any existing *http.Client
// (set it as the Transport, keep the client's own Timeout/etc).
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// Bounded, conservative defaults: a dead upstream gives up in ~1s of total
// backoff, not minutes. Override per client with WithMaxAttempts/WithBaseDelay.
const (
	DefaultMaxAttempts = uint(4)
	DefaultBaseDelay   = 200 * time.Millisecond
	DefaultMaxDelay    = 5 * time.Second
)

// idempotencyKey marks a request's context as safe to retry under the
// GET/HEAD policy despite an unsafe HTTP method - for a call whose repeat
// has no unwanted side effect (e.g. regenerating an LLM completion).
type idempotencyKey struct{}

// WithIdempotent marks ctx so a request built from it is retried as freely
// as GET/HEAD even if its method is POST/PATCH/PUT/DELETE. Use only when a
// duplicate send is known to be harmless - never for a call that creates or
// mutates state visible to someone else (a comment, a review, a merge).
func WithIdempotent(ctx context.Context) context.Context {
	return context.WithValue(ctx, idempotencyKey{}, true)
}

func isIdempotent(ctx context.Context) bool {
	v, _ := ctx.Value(idempotencyKey{}).(bool)
	return v
}

// Option configures a resilient Transport.
type Option func(*transport)

// WithMaxAttempts bounds the total number of attempts (including the first).
func WithMaxAttempts(n uint) Option { return func(t *transport) { t.maxAttempts = n } }

// WithBaseDelay sets the initial exponential-backoff interval.
func WithBaseDelay(d time.Duration) Option { return func(t *transport) { t.baseDelay = d } }

// WithMaxDelay caps the exponential-backoff interval.
func WithMaxDelay(d time.Duration) Option { return func(t *transport) { t.maxDelay = d } }

type transport struct {
	next        http.RoundTripper
	maxAttempts uint
	baseDelay   time.Duration
	maxDelay    time.Duration
}

// NewTransport wraps next (http.DefaultTransport if nil) with a method-aware
// retry policy:
//
//   - GET/HEAD (or any request marked via WithIdempotent) retry on connection
//     errors, timeouts, 429, and 5xx.
//   - Every other method retries only on errors that prove the request never
//     reached the server (connection refused, DNS failure) - never on a
//     mid-flight timeout or a 5xx response, since either could mean the
//     server already processed it.
//
// Retry-After is honoured when present. Attempts are bounded.
func NewTransport(next http.RoundTripper, opts ...Option) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	t := &transport{
		next:        next,
		maxAttempts: DefaultMaxAttempts,
		baseDelay:   DefaultBaseDelay,
		maxDelay:    DefaultMaxDelay,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	freely := req.Method == http.MethodGet || req.Method == http.MethodHead || isIdempotent(req.Context())

	// A body we can't reconstruct can't be safely resent - one attempt only.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return t.next.RoundTrip(req)
	}

	bo := &backoff.ExponentialBackOff{
		InitialInterval:     t.baseDelay,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         t.maxDelay,
	}

	// pending is a response we decided to retry past; its body must be
	// drained/closed before the next attempt reuses the connection.
	var pending *http.Response

	resp, err := backoff.Retry(req.Context(), func() (*http.Response, error) {
		if pending != nil {
			_, _ = io.Copy(io.Discard, pending.Body)
			_ = pending.Body.Close()
			pending = nil
		}

		attempt := req
		if req.GetBody != nil {
			body, gerr := req.GetBody()
			if gerr != nil {
				return nil, backoff.Permanent(gerr)
			}
			attempt = req.Clone(req.Context())
			attempt.Body = body
		}

		resp, rerr := t.next.RoundTrip(attempt)
		if rerr != nil {
			if freely || isProvenUnsent(rerr) {
				return nil, rerr
			}
			return nil, backoff.Permanent(rerr)
		}

		if !freely || !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		pending = resp
		if wait, ok := retryAfter(resp.Header); ok {
			return resp, &backoff.RetryAfterError{Duration: wait}
		}
		return resp, fmt.Errorf("httpx: retryable status %d from %s %s", resp.StatusCode, req.Method, req.URL)
	}, backoff.WithBackOff(bo), backoff.WithMaxTries(t.maxAttempts))

	if err != nil && resp != nil {
		// Retries were exhausted on a status code, not a transport fault -
		// the last response is a real answer; hand it to the caller as-is.
		return resp, nil
	}
	return resp, err
}

// isProvenUnsent reports whether err proves the request never reached the
// server: it failed during connection setup (dial/DNS), not mid-flight.
func isProvenUnsent(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// retryAfter parses the Retry-After header (delta-seconds or an HTTP-date).
func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}
