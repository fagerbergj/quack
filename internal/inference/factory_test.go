package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
)

// providers.<p>.extra_body lands on every chat request's top-level body, streamed or not.
func TestNewModel_ExtraBodyReachesEveryRequest(t *testing.T) {
	routing := map[string]any{"order": []any{"Together"}, "allow_fallbacks": false}
	var got []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body["provider"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m, err := NewModel(config.ProviderConfig{Kind: "openai", Endpoint: srv.URL, APIKey: "k", ExtraBody: map[string]any{"provider": routing}}, "m", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	for _, stream := range []bool{false, true} {
		for range m.GenerateContent(context.Background(), req, stream) {
		}
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], routing) || !reflect.DeepEqual(got[1], routing) {
		t.Fatalf("provider fields = %v, want %v on both requests", got, routing)
	}
}
