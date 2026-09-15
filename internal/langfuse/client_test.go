package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "pub", "secret", WithHTTPClient(srv.Client()))
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
		if r.URL.Path != "/api/public/v2/prompts/system/foo" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("label"); got != "production" {
			t.Errorf("label = %q, want production", got)
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "pub" || p != "secret" {
			t.Errorf("basic auth = %q/%q ok=%v", u, p, ok)
		}
		writeJSON(w, promptWire{
			Name: "system/foo", Version: 3, Type: "text",
			Prompt: rawString("hello"), Config: map[string]any{"model": "x"},
			Labels: []string{"production"}, CommitMessage: "operator edit",
		})
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
		writeJSON(w, promptWire{Name: "x", Version: 2, Type: "text", Prompt: rawString("v2")})
	})
	p, found, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{Version: 2})
	if err != nil || !found || p.Body != "v2" {
		t.Fatalf("p=%+v found=%v err=%v", p, found, err)
	}
}

func TestGetPromptChatRejected(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, promptWire{Name: "chatty", Version: 1, Type: "chat", Prompt: json.RawMessage(`[{"role":"user","content":"hi"}]`)})
	})
	_, _, err := c.GetPrompt(context.Background(), "chatty", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error for chat prompt")
	}
}

func TestGetPrompt5xxTransient(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, _, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if !IsTransient(err) {
		t.Fatalf("want transient error, got %v", err)
	}
}

func TestGetPrompt4xxNotTransient(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	})
	_, _, err := c.GetPrompt(context.Background(), "x", GetPromptOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if IsTransient(err) {
		t.Fatalf("want non-transient error, got %v", err)
	}
}

func TestCreatePrompt(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/public/v2/prompts" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "text" || body["name"] != "system/foo" || body["commitMessage"] != "quack-seed abc" {
			t.Errorf("body = %+v", body)
		}
		writeJSON(w, promptWire{Name: "system/foo", Version: 1, Type: "text", Prompt: rawString("body"), CommitMessage: "quack-seed abc"})
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
