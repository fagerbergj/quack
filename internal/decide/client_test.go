package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
)

const okBody = `{"model":"clef","answers":{
 "accept":{"type":"noul","noul":0.95},
 "team":{"type":"choice","choice":"billing","probabilities":{"billing":0.7,"shipping":0.3}},
 "tone":{"type":"score","score":1.4,"probabilities":{"0":0.1,"1":0.4,"2":0.5}}},
 "usage":{"input_tokens":321,"output_tokens":0},"latency_ms":512.5,"batch_size":1}`

// fakeServer answers /v1/systemone with status/body per call and counts calls.
func fakeServer(t *testing.T, calls *atomic.Int32, respond func(n int32, w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		respond(n, w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(url string, timeout time.Duration, maxTokens int) *Client {
	return NewClient(config.DecisionHandler{URL: url + "/", Model: "clef", Timeout: timeout, MaxInputTokens: maxTokens})
}

func TestAskNormalisesEveryQuestionType(t *testing.T) {
	var calls atomic.Int32
	var sent map[string]any
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, body map[string]any) {
		sent = body
		_, _ = w.Write([]byte(okBody))
	})
	qs := map[string]Question{"accept": {Type: "noul", Instructions: "ok?"}}
	r, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "the state", qs)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if sent["model"] != "clef" || sent["state"] != "the state" || sent["questions"] == nil {
		t.Errorf("request body = %v", sent)
	}
	if got := r.Answers["accept"]; got["true"] != 0.95 || got["false"] < 0.0499 || got["false"] > 0.0501 {
		t.Errorf("noul = %v, want true 0.95 / false 0.05", got)
	}
	if r.Answers["team"]["billing"] != 0.7 || r.Answers["tone"]["2"] != 0.5 {
		t.Errorf("choice/score = %v", r.Answers)
	}
	if r.InputTokens != 321 || r.ServerMS != 512.5 || r.RequestBytes == 0 {
		t.Errorf("usage = %+v", r)
	}
}

func TestAsk413IsTooLargeAndNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"detail":"input is 9000 tokens"}`))
	})
	_, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "9000 tokens") {
		t.Errorf("err = %v, want ErrTooLarge with the server's detail", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (413 is permanent)", calls.Load())
	}
}

func TestAskGuardSkipsOversizedInputWithoutCalling(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(okBody)) })
	r, err := newTestClient(srv.URL, time.Second, 10).Ask(context.Background(), strings.Repeat("x", 400), nil)
	if !errors.Is(err, ErrTooLarge) || calls.Load() != 0 || r.RequestBytes < 400 {
		t.Errorf("err = %v calls = %d bytes = %d, want ErrTooLarge before any call", err, calls.Load(), r.RequestBytes)
	}
}

func TestAskRetriesTransient503Once(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(n int32, w http.ResponseWriter, _ map[string]any) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(okBody))
	})
	if _, err := newTestClient(srv.URL, 5*time.Second, 0).Ask(context.Background(), "s", nil); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestAsk422IsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"q: criteria must not be empty"}`))
	})
	_, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil)
	if err == nil || errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "422") || calls.Load() != 1 {
		t.Errorf("err = %v calls = %d, want one 422 error", err, calls.Load())
	}
}

func TestAskTimesOut(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) { <-release })
	defer close(release)
	start := time.Now()
	_, err := newTestClient(srv.URL, 50*time.Millisecond, 0).Ask(context.Background(), "s", nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v, the timeout must bound the whole call", time.Since(start))
	}
}

func TestAskServiceDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := newTestClient(url, time.Second, 0).Ask(context.Background(), "s", nil); err == nil {
		t.Error("Ask against a closed server returned no error")
	}
}

func TestAskBadJSON(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte("{")) })
	if _, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil); err == nil {
		t.Error("a malformed body must be an error")
	}
}

// TestAskGuardReadsUnescapedBytes: HTML-escaping "<" (6 bytes each) would push this under-cap input over it.
func TestAskGuardReadsUnescapedBytes(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(okBody)) })
	if _, err := newTestClient(srv.URL, time.Second, 200).Ask(context.Background(), strings.Repeat("<", 600), nil); err != nil || calls.Load() != 1 {
		t.Errorf("err = %v calls = %d, want the call made", err, calls.Load())
	}
}

func TestAskLlamaCpp500TooLargeIsTooLargeAndNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"input (9000 tokens) is too large to process. increase the physical batch size"}}`))
	})
	_, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "9000 tokens") {
		t.Errorf("err = %v, want ErrTooLarge with the server's detail", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestAskOther500IsStillRetried(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(n int32, w http.ResponseWriter, _ map[string]any) {
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_, _ = w.Write([]byte(okBody))
	})
	if _, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil); err != nil || calls.Load() != 2 {
		t.Errorf("err = %v, calls = %d, want success on the second call", err, calls.Load())
	}
}

func TestAskLlamaCpp400IsLoggedAndNotRetried(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad question type","type":"invalid_request_error"}}`))
	})
	_, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil)
	if err == nil || errors.Is(err, ErrTooLarge) || calls.Load() != 1 {
		t.Errorf("err = %v, calls = %d, want a plain error after one call", err, calls.Load())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "bad question type") {
		t.Errorf("log = %q, want a warn with the body", logs.String())
	}
}

func TestAskLlamaCppWithoutLatencyMS(t *testing.T) {
	var calls atomic.Int32
	srv := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte(`{"answers":{"accept":{"type":"noul","noul":0.9,"confidence":0.1}},"usage":{"input_tokens":77}}`))
	})
	r, err := newTestClient(srv.URL, time.Second, 0).Ask(context.Background(), "s", nil)
	if err != nil || r.ServerMS != 0 || r.InputTokens != 77 || r.Answers["accept"]["true"] != 0.9 {
		t.Errorf("reply = %+v, err = %v", r, err)
	}
	b, _ := json.Marshal(payload(Result{ServerMS: r.ServerMS, InputTokens: r.InputTokens}, ""))
	if strings.Contains(string(b), "server_ms") || !strings.Contains(string(b), `"input_tokens":77`) {
		t.Errorf("payload = %s, want server_ms omitted and input_tokens kept", b)
	}
}
