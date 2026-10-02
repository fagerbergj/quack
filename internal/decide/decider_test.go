package decide

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/otelobs"
)

var testAccept = Register(Point{
	ID: "test.accept", Primary: "accept", Restrictive: []string{"false"}, Replaces: "test_judge",
	Questions: map[string]Question{"accept": {Type: "noul", Instructions: "accept?"}, "extra": {Type: "noul"}},
}, func(top string) bool { return top == "true" })

var testObserveOnly = Register(Point{
	ID: "test.observe_only", Primary: "q", Modes: []string{config.DecisionModeObserve},
	Questions: map[string]Question{"q": {Type: "noul"}},
}, func(top string) string { return top })

// noulServer answers every request with accept = p, and records each body.
func noulServer(t *testing.T, calls *atomic.Int32, p float64, bodies chan<- map[string]any) string {
	t.Helper()
	return fakeServer(t, calls, func(_ int32, w http.ResponseWriter, body map[string]any) {
		if bodies != nil {
			bodies <- body
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"accept": map[string]any{"type": "noul", "noul": p}, "extra": map[string]any{"type": "noul", "noul": 0.5},
			"q": map[string]any{"type": "noul", "noul": p}}, "usage": map[string]any{"input_tokens": 42}, "latency_ms": 7.0})
	}).URL
}

func newDecider(t *testing.T, url, pointID, mode string, actAt float64) *Decider {
	t.Helper()
	d, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"p": {URL: url, Model: "m", Timeout: 2 * time.Second}},
		Points:   map[string]config.DecisionPoint{pointID: {Enabled: true, Handler: "p", Mode: mode, ActAt: actAt, Fail: config.DecisionFailOpen}},
	})
	if err != nil || d == nil {
		t.Fatalf("New: %v %v", d, err)
	}
	return d
}

func TestPolicy(t *testing.T) {
	for _, c := range []struct {
		name, mode    string
		p             float64
		want          Outcome
		choose, guard bool // Choose(false) and Guard(true) on the parsed answer
	}{
		{"decide confident yes acts", config.DecisionModeDecide, 0.95, OutcomeAct, true, true},
		{"decide exactly at act_at acts", config.DecisionModeDecide, 0.9, OutcomeAct, true, true},
		{"decide below act_at falls back", config.DecisionModeDecide, 0.85, OutcomeFallback, false, true},
		{"decide confident no acts", config.DecisionModeDecide, 0.02, OutcomeAct, false, true},
		{"guard confident restrictive answer restricts", config.DecisionModeGuard, 0.05, OutcomeRestrict, false, false},
		{"guard never widens", config.DecisionModeGuard, 0.99, OutcomePass, false, true},
		{"guard below act_at passes", config.DecisionModeGuard, 0.2, OutcomePass, false, true},
		{"observe never acts", config.DecisionModeObserve, 0.01, OutcomeObserve, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			d := newDecider(t, noulServer(t, &calls, c.p, nil), testAccept.ID, c.mode, 0.9)
			x := testAccept.Decide(context.Background(), d, "state", true)
			if x.Err != nil || x.Outcome != c.want {
				t.Fatalf("outcome = %s err = %v, want %s", x.Outcome, x.Err, c.want)
			}
			if got := x.Choose(false); got != c.choose {
				t.Errorf("Choose(false) = %v, want %v", got, c.choose)
			}
			if got := x.Guard(true); got != c.guard {
				t.Errorf("Guard(true) = %v, want %v", got, c.guard)
			}
		})
	}
}

func TestGuardCannotWidenARestrictiveCurrentValue(t *testing.T) {
	var calls atomic.Int32
	d := newDecider(t, noulServer(t, &calls, 0.99, nil), testAccept.ID, config.DecisionModeGuard, 0.9)
	if got := testAccept.Decide(context.Background(), d, "s", false).Guard(false); got {
		t.Error("a confident accept turned the caller's reject into an accept")
	}
}

func TestDisabledPointMakesNoCall(t *testing.T) {
	var calls atomic.Int32
	url := noulServer(t, &calls, 0.9, nil)
	d := newDecider(t, url, "test.observe_only", config.DecisionModeObserve, 0.9)
	var nilDecider *Decider
	for name, r := range map[string]Result{
		"other point": d.Decide(context.Background(), testAccept.ID, "s", ""),
		"nil decider": nilDecider.Decide(context.Background(), testAccept.ID, "s", ""),
		"unknown id":  d.Decide(context.Background(), "nope", "s", ""),
	} {
		if !errors.Is(r.Err, ErrDisabled) || r.Outcome != OutcomeDisabled || r.Act() || r.Restricts() {
			t.Errorf("%s: %+v, want disabled", name, r)
		}
	}
	<-nilDecider.Observe(context.Background(), testAccept.Point, "s")("true")
	if calls.Load() != 0 {
		t.Errorf("calls = %d, want 0", calls.Load())
	}
}

func TestServiceDownIsNoDecision(t *testing.T) {
	d := newDecider(t, "http://127.0.0.1:1", testAccept.ID, config.DecisionModeDecide, 0.5)
	x := testAccept.Decide(context.Background(), d, "s", true)
	if x.Err == nil || x.Outcome != OutcomeUnavailable || x.Act() || x.Choose(true) != true || x.Guard(true) != true {
		t.Errorf("%+v, want unavailable with the caller's values kept", x)
	}
}

func TestMissingPrimaryAnswerIsNoDecision(t *testing.T) {
	var calls atomic.Int32
	url := fakeServer(t, &calls, func(_ int32, w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte(`{"answers":{"extra":{"noul":0.9}}}`))
	}).URL
	d := newDecider(t, url, testAccept.ID, config.DecisionModeDecide, 0.5)
	if r := d.Decide(context.Background(), testAccept.ID, "s", ""); r.Err == nil || r.Outcome != OutcomeUnavailable {
		t.Errorf("%+v, want unavailable", r)
	}
}

func TestNewValidatesAgainstRegistry(t *testing.T) {
	cfg := func(id, mode string, qs map[string]config.DecisionQuestion) config.DecisionsConfig {
		return config.DecisionsConfig{
			Handlers: map[string]config.DecisionHandler{"p": {URL: "http://x"}},
			Points:   map[string]config.DecisionPoint{id: {Enabled: true, Handler: "p", Mode: mode, Questions: qs}},
		}
	}
	for _, c := range []struct {
		name string
		cfg  config.DecisionsConfig
		want string
	}{
		{"unknown point", cfg("nope", "observe", nil), "no such point"},
		{"unsupported mode", cfg("test.observe_only", "decide", nil), `mode "decide" is not supported`},
		{"unknown question", cfg("test.accept", "observe", map[string]config.DecisionQuestion{"zzz": {}}), "no such question"},
	} {
		if _, err := New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	closed := cfg("test.observe_only", "observe", nil)
	closed.Points["test.observe_only"] = config.DecisionPoint{Enabled: true, Handler: "p", Mode: "observe", Fail: config.DecisionFailClosed}
	if _, err := New(closed); err == nil || !strings.Contains(err.Error(), "restrictive answer") {
		t.Errorf("fail closed without a restrictive answer: err = %v", err)
	}
	if d, err := New(cfg("ext:github/intent", "decide", nil)); err != nil || !d.Enabled("ext:github/intent") {
		t.Errorf("extension point: %v %v", d, err)
	}
	off := cfg("nope", "observe", nil)
	off.Points["nope"] = config.DecisionPoint{}
	if d, err := New(off); d != nil || err != nil {
		t.Errorf("all points disabled: %v %v, want nil decider", d, err)
	}
}

func TestQuestionOverridesReachTheRequest(t *testing.T) {
	var calls atomic.Int32
	bodies := make(chan map[string]any, 1)
	url := noulServer(t, &calls, 0.9, bodies)
	d, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"p": {URL: url, Timeout: time.Second}},
		Points: map[string]config.DecisionPoint{testAccept.ID: {Enabled: true, Handler: "p", Mode: "observe", ActAt: 0.9,
			Questions: map[string]config.DecisionQuestion{"accept": {Instructions: "overridden", Criteria: map[string]any{"true": "yes"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Decide(context.Background(), testAccept.ID, "s", "")
	qs := (<-bodies)["questions"].(map[string]any)
	acc := qs["accept"].(map[string]any)
	if acc["instructions"] != "overridden" || acc["type"] != "noul" || acc["criteria"] == nil {
		t.Errorf("accept = %v", acc)
	}
	if qs["extra"].(map[string]any)["type"] != "noul" {
		t.Errorf("extra question lost: %v", qs)
	}
	if testAccept.Questions["accept"].Instructions != "accept?" {
		t.Error("an override mutated the registered point")
	}
}

// ledgerCapture routes decision log records through the production ledger exporter into a MemStore.
func ledgerCapture(t *testing.T) *ledgertest.MemStore {
	t.Helper()
	mem := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
	t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
	return mem
}

func decisionEntries(t *testing.T, mem *ledgertest.MemStore, chat string) []ledger.DecisionPayload {
	t.Helper()
	entries, err := mem.ReadEntries(context.Background(), chat, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.DecisionPayload
	for _, e := range entries {
		if e.Kind != ledger.KindDecision {
			continue
		}
		var p ledger.DecisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestRecordingPayload(t *testing.T) {
	mem := ledgerCapture(t)
	var calls atomic.Int32
	d := newDecider(t, noulServer(t, &calls, 0.97, nil), testAccept.ID, config.DecisionModeDecide, 0.9)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-1", Node: "n1"})
	testAccept.Decide(ctx, d, map[string]string{"plan": "p"}, false)

	got := decisionEntries(t, mem, "chat-1")
	if len(got) != 1 {
		t.Fatalf("decision entries = %d, want 1", len(got))
	}
	p := got[0]
	if p.Point != "test.accept" || p.Mode != "decide" || p.Handler != "p" || p.Outcome != "act" || !p.Confident ||
		p.SkippedStep == nil || *p.SkippedStep != "test_judge" ||
		p.Top != "true" || p.TopP != 0.97 || p.Baseline != "false" || p.InputTokens != 42 || p.ServerMS != 7 ||
		p.RequestBytes == 0 || p.LatencyMS <= 0 || p.Error != "" {
		t.Errorf("payload = %+v", p)
	}
	if p.Probabilities["accept"]["true"] != 0.97 || p.Probabilities["extra"]["true"] != 0.5 {
		t.Errorf("probabilities = %v", p.Probabilities)
	}
	if string(p.State) != `{"plan":"p"}` || !strings.Contains(string(p.Questions), `"accept?"`) {
		t.Errorf("state = %s questions = %s", p.State, p.Questions)
	}
	entries, _ := mem.ReadEntries(context.Background(), "chat-1", 0)
	if entries[0].NodeID != "n1" || !ledger.IsObservation(entries[0].Kind) {
		t.Errorf("entry = %+v, want node n1 and an observation kind", entries[0])
	}
}

func TestObserveRecordsBaselineAndNeverActs(t *testing.T) {
	mem := ledgerCapture(t)
	var calls atomic.Int32
	d := newDecider(t, noulServer(t, &calls, 0.01, nil), testObserveOnly.ID, config.DecisionModeObserve, 0.9)
	ctx, cancel := context.WithCancel(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-2"}))
	settle := d.Observe(ctx, testObserveOnly.Point, "s")
	cancel() // the caller's ctx ending must not drop the record
	<-settle("true")

	got := decisionEntries(t, mem, "chat-2")
	if len(got) != 1 || got[0].Outcome != "observe" || got[0].Baseline != "true" || got[0].Top != "false" || !got[0].Confident || got[0].SkippedStep != nil {
		t.Errorf("entries = %+v, want one observe record with baseline true", got)
	}
}

func TestObserveRecordsUnavailable(t *testing.T) {
	mem := ledgerCapture(t)
	d := newDecider(t, "http://127.0.0.1:1", testObserveOnly.ID, config.DecisionModeObserve, 0.9)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-3"})
	<-d.Observe(ctx, testObserveOnly.Point, "s")("false")
	got := decisionEntries(t, mem, "chat-3")
	if len(got) != 1 || got[0].Outcome != "unavailable" || got[0].Error == "" || got[0].Baseline != "false" {
		t.Errorf("entries = %+v, want one unavailable record", got)
	}
}

func TestTopBreaksTiesLexically(t *testing.T) {
	if top, p := top(map[string]float64{"b": 0.5, "a": 0.5}); top != "a" || p != 0.5 {
		t.Errorf("top = %s %v", top, p)
	}
}

// TestObserveOverridesTheConfiguredMode: Observe never applies, so it never records guard/decide outcomes.
func TestObserveOverridesTheConfiguredMode(t *testing.T) {
	mem := ledgerCapture(t)
	var calls atomic.Int32
	d := newDecider(t, noulServer(t, &calls, 0.99, nil), testAccept.ID, config.DecisionModeDecide, 0.9)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-4"})
	<-d.Observe(ctx, testAccept.Point, "s")("false")
	got := decisionEntries(t, mem, "chat-4")
	if len(got) != 1 || got[0].Outcome != "observe" || got[0].Mode != "decide" || !got[0].Confident || got[0].SkippedStep != nil {
		t.Errorf("entries = %+v, want observe outcome with the decide counterfactual and no skipped step", got)
	}
}

func TestGuardFailClosedRestrictsWithoutADecision(t *testing.T) {
	d, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"p": {URL: "http://127.0.0.1:1", Timeout: time.Second}},
		Points: map[string]config.DecisionPoint{testAccept.ID: {Enabled: true, Handler: "p", Mode: config.DecisionModeGuard,
			ActAt: 0.9, Fail: config.DecisionFailClosed}},
	})
	if err != nil {
		t.Fatal(err)
	}
	x := testAccept.Decide(context.Background(), d, "s", true)
	if x.Err == nil || x.Outcome != OutcomeRestrict || x.Guard(true) != false {
		t.Errorf("%+v, want a fail-closed restrict to false", x)
	}
	open := newDecider(t, "http://127.0.0.1:1", testAccept.ID, config.DecisionModeGuard, 0.9)
	if x := testAccept.Decide(context.Background(), open, "s", true); x.Outcome != OutcomeUnavailable || x.Guard(true) != true {
		t.Errorf("%+v, want fail open to keep the caller's value", x)
	}
}

func TestPointTimeoutCapsTheHandler(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	url := fakeServer(t, &calls, func(int32, http.ResponseWriter, map[string]any) { <-release }).URL
	defer close(release)
	d, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"p": {URL: url, Timeout: time.Minute}},
		Points: map[string]config.DecisionPoint{testAccept.ID: {Enabled: true, Handler: "p", Mode: config.DecisionModeDecide,
			ActAt: 0.9, Fail: config.DecisionFailOpen, Timeout: 50 * time.Millisecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := d.Decide(context.Background(), testAccept.ID, "s", ""); !errors.Is(r.Err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v, want the point's 50ms deadline", r.Err, time.Since(start))
	}
}

func TestExtPointIDConfinesAnExtensionToItsNamespace(t *testing.T) {
	for _, c := range []struct{ plugin, name, want string }{
		{"github", "intent", "ext:github/intent"},
		{"github", "ext:github/intent", "ext:github/intent"},
		{"github", "ext:sleeper/intent", ""},
		{"github", "plan.accept", "ext:github/plan.accept"}, // a core-looking name still lands in its own namespace
		{"github", "a/b", ""},
		{"github", "", ""},
	} {
		got, err := ExtPointID(c.plugin, c.name)
		if got != c.want || (c.want == "") != errors.Is(err, ErrNamespace) {
			t.Errorf("ExtPointID(%q, %q) = %q, %v; want %q", c.plugin, c.name, got, err, c.want)
		}
	}
}

func TestRegisterRejectsTheExtNamespace(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register accepted an ext: id")
		}
	}()
	Register(Point{ID: "ext:core/x", Primary: "q", Questions: map[string]Question{"q": {Type: "noul"}}}, func(s string) string { return s })
}
