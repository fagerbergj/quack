// Package decidetest fakes a systemone handler and captures decision records for core points' tests.
package decidetest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// Down is a handler URL nothing listens on.
const Down = "http://127.0.0.1:1"

// Fake is a systemone handler answering one noul question.
type Fake struct {
	URL                          string
	Calls, InFlight, MaxInFlight atomic.Int32
}

// Server answers noul question q with p after delay, and sends each request's state to states when non-nil.
func Server(t testing.TB, q string, p float64, delay time.Duration, states chan<- json.RawMessage) *Fake {
	t.Helper()
	f := &Fake{}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Calls.Add(1)
		n := f.InFlight.Add(1)
		defer f.InFlight.Add(-1)
		for m := f.MaxInFlight.Load(); n > m && !f.MaxInFlight.CompareAndSwap(m, n); m = f.MaxInFlight.Load() {
		}
		var body struct{ State json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if states != nil {
			select {
			case states <- body.State:
			case <-done:
				return
			}
		}
		select {
		case <-time.After(delay):
		case <-done:
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{q: map[string]any{"noul": p}}})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) }) // runs first: frees a delayed handler so Close can return
	f.URL = srv.URL
	return f
}

// Decider enables pointID in observe mode against url.
func Decider(t testing.TB, url, pointID string) *decide.Decider {
	t.Helper()
	return DeciderWithCap(t, url, pointID, 0)
}

// DeciderWithCap is Decider with the handler's max_input_tokens set.
func DeciderWithCap(t testing.TB, url, pointID string, maxInputTokens int) *decide.Decider {
	t.Helper()
	d, err := decide.New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: url, Model: "clef", Timeout: 2 * time.Second, MaxInputTokens: maxInputTokens}},
		Points:   map[string]config.DecisionPoint{pointID: {Enabled: true, Handler: "clef", Mode: config.DecisionModeObserve, ActAt: 0.9, Fail: config.DecisionFailOpen}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Capture routes decision records through the production ledger exporter into a MemStore.
func Capture(t testing.TB) *ledgertest.MemStore {
	t.Helper()
	mem := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
	t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
	return mem
}

// Records waits up to 5s for chat to hold n decision records and returns what it holds.
func Records(t testing.TB, mem *ledgertest.MemStore, chat string, n int) []ledger.DecisionPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := mem.ReadEntries(context.Background(), chat, 0)
		var out []ledger.DecisionPayload
		for _, e := range entries {
			if e.Kind == ledger.KindDecision {
				var p ledger.DecisionPayload
				_ = json.Unmarshal(e.Payload, &p)
				out = append(out, p)
			}
		}
		if len(out) >= n || time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}
