package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
)

func TestExtDecideConfinesAnExtensionToItsNamespace(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"write": map[string]any{"noul": 0.97}}})
	}))
	defer srv.Close()
	d, err := decide.New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: srv.URL, Timeout: time.Second}},
		Points: map[string]config.DecisionPoint{
			"ext:github/intent":  {Enabled: true, Handler: "clef", Mode: "decide", ActAt: 0.9, Fail: "open"},
			"ext:sleeper/intent": {Enabled: true, Handler: "clef", Mode: "decide", ActAt: 0.9, Fail: "open"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	point := func(id string) decide.Point {
		return decide.Point{ID: id, Primary: "write", Questions: map[string]decide.Question{"write": {Type: "noul"}}}
	}
	github := extDecide(d, "github")

	r, err := github(context.Background(), point("intent"), "push it now", "false")
	if err != nil || r.Point != "ext:github/intent" || !r.Act() || r.Top != "true" {
		t.Errorf("own point: %+v %v, want ext:github/intent acting on true", r, err)
	}
	if _, err := github(context.Background(), point("ext:sleeper/intent"), "s", ""); !errors.Is(err, decide.ErrNamespace) {
		t.Errorf("another extension's point: err = %v, want ErrNamespace", err)
	}
	if _, err := github(context.Background(), point("review"), "s", ""); !errors.Is(err, decide.ErrDisabled) {
		t.Errorf("unconfigured point: err = %v, want ErrDisabled", err)
	}
	observeOnly := point("intent")
	observeOnly.Modes = []string{"observe"}
	if r, err := github(context.Background(), observeOnly, "s", ""); !errors.Is(err, decide.ErrDisabled) || r.Act() {
		t.Errorf("mode the extension doesn't implement: %+v %v, want ErrDisabled", r, err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want only the own, enabled point to call out", calls.Load())
	}
}
