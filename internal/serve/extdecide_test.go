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

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

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
	req := func(id string) extsdk.DecideRequest {
		return extsdk.DecideRequest{Point: id, Primary: "write", Questions: map[string]extsdk.DecisionQuestion{"write": {Type: "noul"}}, State: "push it now", Baseline: "false"}
	}
	github := extDecide(d, "github")

	r, err := github(context.Background(), req("intent"))
	if err != nil || !r.Act || r.Top != "true" || r.Outcome != "act" || r.Probabilities["write"]["true"] != 0.97 {
		t.Errorf("own point: %+v %v, want ext:github/intent acting on true", r, err)
	}
	if _, err := github(context.Background(), req("ext:sleeper/intent")); !errors.Is(err, decide.ErrNamespace) {
		t.Errorf("another extension's point: err = %v, want ErrNamespace", err)
	}
	if _, err := github(context.Background(), req("review")); !errors.Is(err, decide.ErrDisabled) {
		t.Errorf("unconfigured point: err = %v, want ErrDisabled", err)
	}
	var nilDecider *decide.Decider
	if _, err := extDecide(nilDecider, "github")(context.Background(), req("intent")); !errors.Is(err, decide.ErrDisabled) {
		t.Errorf("nil decider: err = %v, want ErrDisabled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want only the own, enabled point to call out", calls.Load())
	}
}
