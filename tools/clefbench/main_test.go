package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
)

func TestBenchSendsEvidencePerModeAndScores(t *testing.T) {
	s, err := load("testdata/cases.json")
	if err != nil || len(s.Cases) != 8 {
		t.Fatalf("load: %d cases, %v", len(s.Cases), err)
	}
	for _, mode := range []string{"text", "image", "both"} {
		var bad []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				State  map[string]any `json:"state"`
				Images []string       `json:"images"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, hasEls := body.State["interactive_elements"]
			hasImg := len(body.Images) == 1 && strings.HasPrefix(body.Images[0], "data:image/jpeg;base64,/9j/")
			if hasEls != (mode != "image") || hasImg != (mode != "text") {
				bad = append(bad, body.State["url"].(string))
			}
			_, _ = w.Write([]byte(`{"answers":{"target":{"type":"choice","probabilities":{"Linux":0.95,"Okay":0.05}}},"usage":{"input_tokens":100}}`))
		}))
		c := decide.NewClient(config.DecisionHandler{URL: srv.URL, Model: "clef", Timeout: 5 * time.Second})
		var out strings.Builder
		report(&out, run(context.Background(), c, s, "testdata", mode), 0.9)
		srv.Close()
		if len(bad) > 0 {
			t.Errorf("%s: wrong evidence for %v", mode, bad)
		}
		if !strings.Contains(out.String(), "accuracy 1/8 answered (8 cases); p>=0.90: 1 right, 7 wrong") {
			t.Errorf("%s report:\n%s", mode, out.String())
		}
	}
}
