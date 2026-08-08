package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastOpts keeps retry tests from waiting out real backoff.
func fastOpts(opts ...Option) []Option {
	return append([]Option{WithBaseDelay(time.Millisecond), WithMaxDelay(5 * time.Millisecond)}, opts...)
}

// --- Done-when's two headline scenarios, over a real httptest.Server ---

// TestGET502ThenSucceeds pins the Done-when requirement: a 502-then-200
// sequence succeeds.
func TestGET502ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(nil, fastOpts()...)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2 (one 502, one retry that succeeds)", got)
	}
}

// TestPOSTMaybeProcessedIsNotRetried pins the Done-when requirement that
// matters most: a POST that may have already been processed must not be
// retried, even on a 502 - a naive retry here would double-post.
func TestPOSTMaybeProcessedIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(nil, fastOpts()...)}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 passed through unretried", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hit %d times, want exactly 1 (a POST that may have been processed must not be retried)", got)
	}
}

// --- Method-aware policy matrix, against a scripted stub transport ---

type stubResult struct {
	resp *http.Response
	err  error
}

type stubTransport struct {
	results []stubResult
	calls   int
}

func (s *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	i := s.calls
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.calls++
	return s.results[i].resp, s.results[i].err
}

func statusResp(code int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(""))}
}

// dialErr simulates a connection-refused failure during connection setup -
// proof the request body was never sent.
func dialErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// timeoutErr simulates a mid-flight timeout (e.g. the server accepted the
// connection but didn't answer in time) - NOT proof of non-delivery.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func doReq(t *testing.T, tr http.RoundTripper, method string, ctx context.Context) (*http.Response, error) {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://example.invalid/x", body)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	return tr.RoundTrip(req)
}

func TestGETRetriesOnConnectionErrorThenSucceeds(t *testing.T) {
	stub := &stubTransport{results: []stubResult{
		{err: dialErr()},
		{err: dialErr()},
		{resp: statusResp(200, nil)},
	}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 || stub.calls != 3 {
		t.Errorf("status=%d calls=%d, want 200/3", resp.StatusCode, stub.calls)
	}
}

func TestGETRetriesOnMidFlightTimeout(t *testing.T) {
	stub := &stubTransport{results: []stubResult{
		{err: timeoutErr{}},
		{resp: statusResp(200, nil)},
	}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 || stub.calls != 2 {
		t.Errorf("status=%d calls=%d, want 200/2 (GET retries a mid-flight timeout freely)", resp.StatusCode, stub.calls)
	}
}

func TestPOSTRetriedOnConnectionRefused(t *testing.T) {
	stub := &stubTransport{results: []stubResult{
		{err: dialErr()},
		{resp: statusResp(200, nil)},
	}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodPost, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 || stub.calls != 2 {
		t.Errorf("status=%d calls=%d, want 200/2 (dial failure proves the POST was never sent)", resp.StatusCode, stub.calls)
	}
}

func TestPOSTNotRetriedOnMidFlightTimeout(t *testing.T) {
	stub := &stubTransport{results: []stubResult{{err: timeoutErr{}}}}
	rt := NewTransport(stub, fastOpts()...)
	_, err := doReq(t, rt, http.MethodPost, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1 (a mid-flight timeout can't prove the POST was never processed)", stub.calls)
	}
}

func TestPOSTNotRetriedOn5xxStatus(t *testing.T) {
	stub := &stubTransport{results: []stubResult{{resp: statusResp(503, nil)}}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodPost, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 503 || stub.calls != 1 {
		t.Errorf("status=%d calls=%d, want 503/1 (a response proves the server saw the POST)", resp.StatusCode, stub.calls)
	}
}

func TestIdempotentOptInRetriesPOSTOn5xx(t *testing.T) {
	stub := &stubTransport{results: []stubResult{
		{resp: statusResp(502, nil)},
		{resp: statusResp(200, nil)},
	}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodPost, WithIdempotent(context.Background()))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 || stub.calls != 2 {
		t.Errorf("status=%d calls=%d, want 200/2 (an opted-in idempotent POST retries like GET)", resp.StatusCode, stub.calls)
	}
}

func Test404NeverRetried(t *testing.T) {
	stub := &stubTransport{results: []stubResult{{resp: statusResp(404, nil)}}}
	rt := NewTransport(stub, fastOpts()...)
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 404 || stub.calls != 1 {
		t.Errorf("status=%d calls=%d, want 404/1 (404 is a semantic race, not a transport fault - never blanket-retried)", resp.StatusCode, stub.calls)
	}
}

func TestBoundedAttempts(t *testing.T) {
	stub := &stubTransport{results: []stubResult{{resp: statusResp(500, nil)}}} // always fails
	rt := NewTransport(stub, append(fastOpts(), WithMaxAttempts(3))...)
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want the last response (500) once retries are exhausted", resp.StatusCode)
	}
	if stub.calls != 3 {
		t.Errorf("calls = %d, want exactly 3 (WithMaxAttempts bound)", stub.calls)
	}
}

func TestUnreplayableBodyAttemptedOnce(t *testing.T) {
	stub := &stubTransport{results: []stubResult{{err: dialErr()}, {resp: statusResp(200, nil)}}}
	rt := NewTransport(stub, fastOpts()...)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.invalid/x", io.NopCloser(strings.NewReader(`{}`)))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.GetBody = nil // simulate a body http.NewRequest couldn't make replayable

	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected error, got nil")
	}
	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1 (a body that can't be reconstructed must not be resent)", stub.calls)
	}
}

// --- Retry-After ---

func TestRetryAfterSecondsHonoured(t *testing.T) {
	stub := &stubTransport{results: []stubResult{
		{resp: statusResp(429, map[string]string{"Retry-After": "0"})},
		{resp: statusResp(200, nil)},
	}}
	// A large default backoff: if Retry-After weren't honoured, this would take seconds.
	rt := NewTransport(stub, WithBaseDelay(2*time.Second), WithMaxDelay(2*time.Second))
	start := time.Now()
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want well under the 2s exponential base (Retry-After: 0 should have been honoured)", elapsed)
	}
}

func TestRetryAfterHTTPDateHonoured(t *testing.T) {
	when := time.Now().Add(50 * time.Millisecond).UTC().Format(http.TimeFormat)
	stub := &stubTransport{results: []stubResult{
		{resp: statusResp(429, map[string]string{"Retry-After": when})},
		{resp: statusResp(200, nil)},
	}}
	rt := NewTransport(stub, WithBaseDelay(2*time.Second), WithMaxDelay(2*time.Second))
	start := time.Now()
	resp, err := doReq(t, rt, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want well under the 2s exponential base (Retry-After HTTP-date should have been honoured)", elapsed)
	}
}
