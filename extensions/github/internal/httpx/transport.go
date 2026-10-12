// Package httpx is a vendored copy of quack's internal/httpx retrying RoundTripper; extensions can't
// import quack internals. Promote it into the SDK if a second extension needs it.
package httpx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// Bounded defaults: a dead upstream gives up in ~1s of total backoff, not minutes.
// Production passes zero delays to take these; tests shrink them.
const (
	DefaultMaxAttempts = uint(4)
	DefaultBaseDelay   = 200 * time.Millisecond
	DefaultMaxDelay    = 5 * time.Second
)

type transport struct {
	next      http.RoundTripper
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewTransport wraps next (default if nil) with bounded retries honouring Retry-After: GET/HEAD on any
// fault, 429 or 5xx; other methods only when the request provably never reached the server.
func NewTransport(next http.RoundTripper, baseDelay, maxDelay time.Duration) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if baseDelay == 0 {
		baseDelay = DefaultBaseDelay
	}
	if maxDelay == 0 {
		maxDelay = DefaultMaxDelay
	}
	return &transport{next: next, baseDelay: baseDelay, maxDelay: maxDelay}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	freely := req.Method == http.MethodGet || req.Method == http.MethodHead

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

		attempt, gerr := rewound(req)
		if gerr != nil {
			return nil, backoff.Permanent(gerr)
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
	}, backoff.WithBackOff(bo), backoff.WithMaxTries(DefaultMaxAttempts))

	if err != nil && resp != nil {
		// Retries exhausted on a status code: the last response is a real answer.
		return resp, nil
	}
	return resp, err
}

// rewound returns req with a fresh body for one more attempt.
func rewound(req *http.Request) (*http.Request, error) {
	if req.GetBody == nil {
		return req, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	attempt := req.Clone(req.Context())
	attempt.Body = body
	return attempt, nil
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
