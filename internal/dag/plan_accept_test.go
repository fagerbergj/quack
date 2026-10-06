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
	"github.com/fagerbergj/quack/internal/vetting"
)

// planAcceptDecider enables plan.accept against url in observe mode.
func planAcceptDecider(t *testing.T, url string) *decide.Decider {
	return planAcceptPoint(t, url, config.DecisionPoint{Mode: "observe"})
}

// planAcceptPoint enables plan.accept against url with pt's mode, timeout and audit rate.
func planAcceptPoint(t *testing.T, url string, pt config.DecisionPoint) *decide.Decider {
	t.Helper()
	pt.Enabled, pt.Handler, pt.ActAt, pt.Fail = true, "clef", 0.9, "open"
	d, err := decide.New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: url, Model: "clef", Timeout: 2 * time.Second}},
		Points:   map[string]config.DecisionPoint{"plan.accept": pt},
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
				if !strings.Contains(state, "Write a plan.") || !strings.Contains(state, "task: Analyze the repo.") || *lastSummary == "" {
					t.Errorf("state = %q, want the request and the plan's tasks", state)
				}
			}
		})
	}
}

// TestPlanAcceptStateCarriesFactsNotEarlierOutputs: earlier nodes' outputs and quack's partial-plan
// hint reach the plan judge but never plan.accept's state, which gets each node's status and output size.
func TestPlanAcceptStateCarriesFactsNotEarlierOutputs(t *testing.T) {
	const sentinel = "CONCLUSION: the GIL is off, ship it"
	states := make(chan string, 1)
	judge, _, _, lastSummary := fakePlanJudge(true, "", nil)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher", DefaultArtifact: "body"}, {Name: "synthesizer"}}, nil, judge)
	p.SetDecisions(planAcceptDecider(t, opposingClef(t, true, states)))
	raw, err := AssignmentsToRawNodes([]Assignment{
		{NodeID: "r", Task: "Check the build.", TaskID: "t1", Result: sentinel},
		{NodeID: "s", Task: "Stopped draft.", TaskID: "t2", Result: "draft " + sentinel, Stopped: true},
		{NodeID: "f", Task: "Failed lookup.", TaskID: "t3"},
		{NodeID: "next", Task: "Summarize.", DependsOn: []string{"r", "s", "f"}},
	}, map[string]string{"r": "web-researcher", "s": "web-researcher", "f": "web-researcher", "next": "synthesizer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Build(context.Background(), raw, nil, nil, nil, "Is free-threading on?", nil, nil); err != nil {
		t.Fatal(err)
	}
	state := <-states
	for _, banned := range []string{sentinel, "ALREADY RAN", "partial plan"} {
		if strings.Contains(state, banned) {
			t.Errorf("state carries %q:\n%s", banned, state)
		}
	}
	for _, want := range []string{"Is free-threading on?", "task: Check the build.",
		"status: done, artifact kind: body, output: 35 bytes", "status: cancelled, artifact kind: body, output: 41 bytes",
		"status: failed, artifact kind: body, output: 0 bytes", "- next (synthesizer) depends on r, s, f\n    task: Summarize.\n    status: pending",
		"delivery: none"} {
		if !strings.Contains(state, want) {
			t.Errorf("state lacks %q:\n%s", want, state)
		}
	}
	if !strings.Contains(*lastSummary, sentinel) || !strings.Contains(*lastSummary, "partial plan") {
		t.Errorf("judge summary = %q, want it unchanged: earlier outputs and the partial-plan note", *lastSummary)
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

func TestPlanAcceptModes(t *testing.T) {
	for mode, ok := range map[string]bool{"observe": true, "decide": true, "guard": false} {
		_, err := decide.New(config.DecisionsConfig{
			Handlers: map[string]config.DecisionHandler{"clef": {URL: "http://x"}},
			Points:   map[string]config.DecisionPoint{"plan.accept": {Enabled: true, Handler: "clef", Mode: mode}},
		})
		if (err == nil) != ok {
			t.Errorf("mode %s: err = %v, want accepted = %v", mode, err, ok)
		}
	}
	if !planAccept.Parse("true") || planAccept.Parse("false") {
		t.Error("plan.accept parses true/false into accept")
	}
}

// clefAccepting answers accept = pTrue; a negative pTrue hangs until the test ends.
func clefAccepting(t *testing.T, pTrue float64) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if pTrue < 0 {
			<-release
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"accept": map[string]any{"noul": pTrue}}})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// gatedJudge is a race-safe plan judge that answers accept once gate is closed.
func gatedJudge(accept bool, gate <-chan struct{}) (vetting.PlanJudge, *atomic.Int32) {
	calls := new(atomic.Int32)
	return func(context.Context, string, string, string) (bool, string, error) {
		calls.Add(1)
		<-gate
		return accept, "add a terminal node", nil
	}, calls
}

// TestPlanAcceptDecide: only a confident accept skips the judge; a confident reject, an unsure or
// missing answer runs it as observe does and records its verdict as the baseline.
func TestPlanAcceptDecide(t *testing.T) {
	for _, c := range []struct {
		name                    string
		pTrue                   float64
		url                     string
		judgeAccepts, wantJudge bool
		outcome, baseline       string
		reason                  bool
	}{
		{"confident accept skips the judge", 0.97, "", false, false, "act", "", false},
		{"confident reject falls back to the judge", 0.02, "", true, true, "fallback", "true", true},
		{"confident reject, judge rejects too", 0.02, "", false, true, "fallback", "false", true},
		{"unsure runs the judge", 0.6, "", false, true, "fallback", "false", false},
		{"service down runs the judge", 0, "http://127.0.0.1:1", true, true, "unavailable", "true", false},
		{"timeout runs the judge", -1, "", true, true, "unavailable", "true", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mem := ledgertest.NewMemStore()
			lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
			t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
			url := c.url
			if url == "" {
				url = clefAccepting(t, c.pTrue)
			}
			open := make(chan struct{})
			close(open)
			judge, calls := gatedJudge(c.judgeAccepts, open)
			p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
			p.SetDecisions(planAcceptPoint(t, url, config.DecisionPoint{Mode: "decide", Timeout: 50 * time.Millisecond, AuditRate: new(0.0)}))
			ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-pd"})
			start := time.Now()
			plan, err := p.Build(ctx, []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze the repo."}},
				nil, nil, nil, "Write a plan.", nil, nil)

			if took := time.Since(start); took > time.Second {
				t.Errorf("Build took %v, want the point's timeout to bound the wait", took)
			}
			if got := calls.Load() == 1; got != c.wantJudge {
				t.Errorf("judge calls = %d, want judge run = %v", calls.Load(), c.wantJudge)
			}
			var rejected *PlanRejectedError
			if wantPlan := !c.wantJudge || c.judgeAccepts; wantPlan != (err == nil && plan != nil) || (!wantPlan && !errors.As(err, &rejected)) {
				t.Fatalf("Build = %v, %v; want plan = %v", plan, err, wantPlan)
			}
			got := waitDecisions(t, mem, "chat-pd")
			if len(got) != 1 {
				t.Fatalf("decision records = %d, want 1", len(got))
			}
			d := got[0]
			if d.Mode != "decide" || d.Outcome != c.outcome || d.Baseline != c.baseline || (d.SkippedStep != nil) != !c.wantJudge || (d.Reason != "") != c.reason {
				t.Errorf("record = %+v, want outcome %s baseline %q", d, c.outcome, c.baseline)
			}
		})
	}
}

// TestPlanAcceptDecideAuditsInTheBackground: an audited act returns the plan without waiting for the
// judge, ignores its reject, and records that verdict beside skipped_step.
func TestPlanAcceptDecideAuditsInTheBackground(t *testing.T) {
	mem := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
	t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
	gate := make(chan struct{})
	judge, calls := gatedJudge(false, gate)
	p := NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, judge)
	p.SetDecisions(planAcceptPoint(t, clefAccepting(t, 0.97), config.DecisionPoint{Mode: "decide", AuditRate: new(1.0)}))
	ctx, cancel := context.WithCancel(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-au"}))
	built := make(chan error, 1)
	go func() {
		plan, err := p.Build(ctx, []RawNode{{ID: "explore", Agent: "web-researcher", Task: "Analyze."}}, nil, nil, nil, "m", nil, nil)
		if err == nil && plan == nil {
			err = errors.New("no plan")
		}
		built <- err
	}()
	select {
	case err := <-built:
		if err != nil {
			t.Fatalf("Build: %v; the act should accept the plan", err)
		}
	case <-time.After(time.Second):
		close(gate)
		t.Fatal("Build waited on the audit's judge")
	}
	cancel() // the turn moving on must not cancel the audit
	time.Sleep(50 * time.Millisecond)
	if entries, _ := mem.ReadEntries(context.Background(), "chat-au", 0); len(entries) != 0 {
		t.Fatalf("recorded %+v before the audit's verdict", entries)
	}
	close(gate)
	got := waitDecisions(t, mem, "chat-au")
	if calls.Load() != 1 || len(got) != 1 || got[0].Outcome != "act" || got[0].SkippedStep == nil ||
		*got[0].SkippedStep != "plan_judge" || got[0].Baseline != "false" {
		t.Errorf("judge calls = %d, records = %+v; want one act with skipped_step plan_judge and the audit's baseline false", calls.Load(), got)
	}
}
