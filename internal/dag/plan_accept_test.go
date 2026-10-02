package dag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// planAcceptDecider enables plan.accept against url, in observe as its only mode.
func planAcceptDecider(t *testing.T, url string) *decide.Decider {
	t.Helper()
	d, err := decide.New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: url, Model: "clef", Timeout: 2 * time.Second}},
		Points:   map[string]config.DecisionPoint{"plan.accept": {Enabled: true, Handler: "clef", Mode: "observe", ActAt: 0.9, Fail: "open"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// opposingClef confidently answers the opposite of judgeAccepts, and records each request's state.
func opposingClef(t *testing.T, judgeAccepts bool, states chan<- string) string {
	t.Helper()
	p := 0.99
	if judgeAccepts {
		p = 0.01
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		states <- body.State
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"accept": map[string]any{"noul": p}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitDecisions(t *testing.T, mem *ledgertest.MemStore, chat string) []ledger.DecisionPayload {
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
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPlanAcceptObserveNeverChangesThePlanJudgeFlow: whatever the decision service
// answers, or if it is down, Build's outcome is exactly the plan judge's.
func TestPlanAcceptObserveNeverChangesThePlanJudgeFlow(t *testing.T) {
	judgeErr := errors.New("judge model down")
	for _, c := range []struct {
		name     string
		accept   bool
		judgeErr error
		down     bool
		baseline string
	}{
		{"judge accepts, service rejects", true, nil, false, "true"},
		{"judge rejects, service accepts", false, nil, false, "false"},
		{"judge errors, service up", false, judgeErr, false, ""},
		{"judge accepts, service down", true, nil, true, "true"},
		{"judge rejects, service down", false, nil, true, "false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mem := ledgertest.NewMemStore()
			lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
			t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))

			states := make(chan string, 1)
			url := opposingClef(t, c.accept, states)
			if c.down {
				url = "http://127.0.0.1:1"
			}
			judge, calls, _, lastSummary := fakePlanJudge(c.accept, "add a terminal node", c.judgeErr)
			p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
			p.SetDecisions(planAcceptDecider(t, url))
			ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-pa"})
			plan, err := p.Build(ctx, []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."}},
				nil, nil, nil, "Write a plan.", nil, nil)

			var rejected *PlanRejectedError
			wantPlan := c.accept || c.judgeErr != nil
			if wantPlan && (err != nil || plan == nil) {
				t.Fatalf("Build = %v, %v; the judge allowed this plan", plan, err)
			}
			if !wantPlan && !errors.As(err, &rejected) {
				t.Fatalf("Build err = %v, want the judge's rejection", err)
			}
			if *calls != 1 {
				t.Errorf("judge calls = %d, want 1", *calls)
			}

			got := waitDecisions(t, mem, "chat-pa")
			if len(got) != 1 {
				t.Fatalf("decision records = %d, want 1", len(got))
			}
			d := got[0]
			wantOutcome := "observe"
			if c.down {
				wantOutcome = "unavailable"
			}
			if d.Point != "plan.accept" || d.Outcome != wantOutcome || d.Baseline != c.baseline || d.SkippedStep != nil {
				t.Errorf("record = %+v, want outcome %s baseline %q", d, wantOutcome, c.baseline)
			}
			if !c.down {
				state := <-states
				if !strings.Contains(state, "Write a plan.") || !strings.Contains(state, *lastSummary) {
					t.Errorf("state = %q, want the request and the judge's plan summary", state)
				}
			}
		})
	}
}

// TestPlanAcceptObserveDoesNotWaitForTheService: a hung decision service never delays Build.
func TestPlanAcceptObserveDoesNotWaitForTheService(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started.Store(true)
		<-release
	}))
	defer srv.Close()
	defer close(release)

	judge, _, _, _ := fakePlanJudge(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	p.SetDecisions(planAcceptDecider(t, srv.URL))
	done := make(chan error, 1)
	go func() {
		_, err := p.Build(context.Background(), []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze."}},
			nil, nil, nil, "m", nil, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Build waited on the decision service")
	}
}

// TestPlanAcceptSkippedWithoutAJudge: no judge (or a waived one) means no baseline, so no call.
func TestPlanAcceptSkippedWithoutAJudge(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	nodes := []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze."}}

	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, nil)
	p.SetDecisions(planAcceptDecider(t, srv.URL))
	if _, err := p.Build(context.Background(), nodes, nil, nil, nil, "m", nil, nil); err != nil {
		t.Fatal(err)
	}
	judge, _, _, _ := fakePlanJudge(false, "no", nil)
	p = NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	p.SetDecisions(planAcceptDecider(t, srv.URL))
	if _, err := p.Build(WithPlanJudgeWaived(context.Background()), nodes, nil, nil, nil, "m", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 0 {
		t.Errorf("decision calls = %d, want 0", hits.Load())
	}
}

func TestPlanAcceptIsObserveOnly(t *testing.T) {
	_, err := decide.New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: "http://x"}},
		Points:   map[string]config.DecisionPoint{"plan.accept": {Enabled: true, Handler: "clef", Mode: "guard"}},
	})
	if err == nil {
		t.Error("plan.accept accepted guard mode; its caller only implements observe")
	}
	if !planAccept.Parse("true") || planAccept.Parse("false") {
		t.Error("plan.accept parses true/false into accept")
	}
}
