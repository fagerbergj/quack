package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "pub", "secret", append([]Option{WithHTTPClient(srv.Client())}, opts...)...)
}

// rawPrompt serves the real /api/public/v2/prompts/{name} JSON shape as a raw
// literal rather than a promptWire{} fake, so tests catch a field-shape mismatch.
func rawPrompt(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestGetPromptNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, found, err := c.GetPrompt(context.Background(), "system/foo", GetPromptOpts{Label: "production"})
	if err != nil || found {
		t.Fatalf("got found=%v err=%v, want found=false err=nil", found, err)
	}
}

func TestGetPromptByLabel(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/public/v2/prompts/system%2Ffoo" {
			t.Errorf("escaped path = %q", r.URL.EscapedPath())
		}
		if got := r.URL.Query().Get("label"); got != "production" {
			t.Errorf("label = %q, want production", got)
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "pub" || p != "secret" {
			t.Errorf("basic auth = %q/%q ok=%v", u, p, ok)
		}
		rawPrompt(`{
			"name": "system/foo", "version": 3, "type": "text", "prompt": "hello",
			"config": {"model": "x"}, "labels": ["production", "latest"], "tags": [],
			"commitMessage": "operator edit"
		}`)(w, r)
	})
	p, found, err := c.GetPrompt(context.Background(), "system/foo", GetPromptOpts{Label: "production"})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if p.Body != "hello" || p.Version != 3 || p.Config["model"] != "x" {
		t.Fatalf("got %+v", p)
	}
}

func TestGetPromptByVersion(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("version"); got != "2" {
			t.Errorf("version = %q, want 2", got)
		}
		rawPrompt(`{"name": "x", "version": 2, "type": "text", "prompt": "v2", "config": null, "labels": ["latest"], "tags": null, "commitMessage": null}`)(w, r)
	})
	p, found, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{Version: 2})
	if err != nil || !found || p.Body != "v2" {
		t.Fatalf("p=%+v found=%v err=%v", p, found, err)
	}
}

func TestGetPromptLabelAndVersionRejected(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not make a request")
	})
	_, _, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{Label: "production", Version: 2})
	if err == nil {
		t.Fatal("want error")
	}
	if IsTransient(err) {
		t.Fatalf("want permanent error, got transient: %v", err)
	}
}

func TestGetPromptChatRejected(t *testing.T) {
	c := testClient(t, rawPrompt(`{"name": "chatty", "version": 1, "type": "chat", "prompt": [{"role":"user","content":"hi"}]}`))
	_, _, err := c.GetPrompt(context.Background(), "chatty", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error for chat prompt")
	}
	if IsTransient(err) {
		t.Fatalf("chat-type rejection should be permanent, got transient: %v", err)
	}
}

func TestGetPromptStatusCodes(t *testing.T) {
	cases := []struct {
		status    int
		transient bool
		auth      bool
	}{
		{http.StatusInternalServerError, true, false},
		{http.StatusBadGateway, true, false},
		{http.StatusServiceUnavailable, true, false},
		{http.StatusTooManyRequests, true, false},
		{http.StatusUnauthorized, false, true},
		{http.StatusBadRequest, false, false},
	}
	for _, tc := range cases {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", tc.status)
		})
		_, _, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{})
		if err == nil {
			t.Fatalf("status %d: want error", tc.status)
		}
		if got := IsTransient(err); got != tc.transient {
			t.Errorf("status %d: IsTransient = %v, want %v", tc.status, got, tc.transient)
		}
		if got := IsAuthError(err); got != tc.auth {
			t.Errorf("status %d: IsAuthError = %v, want %v", tc.status, got, tc.auth)
		}
	}
}

func TestGetPromptCancelledContext(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach the server")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := c.GetPrompt(ctx, "x", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if IsTransient(err) {
		t.Fatalf("cancelled context should be permanent, got transient: %v", err)
	}
}

func TestGetPromptDeadlineExceeded(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, _, err := c.GetPrompt(ctx, "x", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if IsTransient(err) {
		t.Fatalf("deadline exceeded should be permanent, got transient: %v", err)
	}
}

func TestCreatePrompt(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/public/v2/prompts" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "text" || body["name"] != "system/foo" || body["commitMessage"] != "quack-seed abc" {
			t.Errorf("body = %+v", body)
		}
		if _, ok := body["config"].(map[string]any); !ok {
			t.Errorf("config = %#v, want object", body["config"])
		}
		tags, ok := body["tags"].([]any)
		if !ok || len(tags) != 1 || tags[0] != SeedTag {
			t.Errorf("tags = %#v", body["tags"])
		}
		rawPrompt(`{"name": "system/foo", "version": 1, "type": "text", "prompt": "body", "commitMessage": "quack-seed abc"}`)(w, r)
	})
	p, err := c.CreatePrompt(context.Background(), CreatePromptRequest{
		Name: "system/foo", Body: "body", Config: map[string]any{}, Tags: []string{SeedTag}, CommitMessage: "quack-seed abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.Body != "body" {
		t.Fatalf("got %+v", p)
	}
}

func TestCreatePromptNilLabelsAndTagsMarshalAsEmptyArrays(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"labels", "tags"} {
			if string(raw[field]) == "null" {
				t.Errorf("%s marshalled as null, want []", field)
			}
		}
		rawPrompt(`{"name": "x", "version": 1, "type": "text", "prompt": "b"}`)(w, r)
	})
	if _, err := c.CreatePrompt(context.Background(), CreatePromptRequest{Name: "x", Body: "b"}); err != nil {
		t.Fatal(err)
	}
}
