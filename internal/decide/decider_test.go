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
	"unicode/utf8"

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

var testActsTrue = Register(Point{
	ID: "test.acts_true", Primary: "accept", Acts: []string{"true"}, Replaces: "test_judge",
	Questions: map[string]Question{"accept": {Type: "noul"}},
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
		name, mode string
		p          float64
		want       Outcome
	}{
		{"decide confident yes acts", config.DecisionModeDecide, 0.95, OutcomeAct},
		{"decide exactly at act_at acts", config.DecisionModeDecide, 0.9, OutcomeAct},
		{"decide below act_at falls back", config.DecisionModeDecide, 0.85, OutcomeFallback},
		{"decide confident no acts", config.DecisionModeDecide, 0.02, OutcomeAct},
		{"guard confident restrictive answer restricts", config.DecisionModeGuard, 0.05, OutcomeRestrict},
		{"guard never widens", config.DecisionModeGuard, 0.99, OutcomePass},
		{"guard below act_at passes", config.DecisionModeGuard, 0.2, OutcomePass},
		{"observe never acts", config.DecisionModeObserve, 0.01, OutcomeObserve},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			d := newDecider(t, noulServer(t, &calls, c.p, nil), testAccept.ID, c.mode, 0.9)
			x := d.DecideWith(context.Background(), testAccept.Point, "state", "true")
			if x.Err != nil || x.Outcome != c.want {
				t.Fatalf("outcome = %s err = %v, want %s", x.Outcome, x.Err, c.want)
			}
		})
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
	x := d.DecideWith(context.Background(), testAccept.Point, "s", "true")
	if x.Err == nil || x.Outcome != OutcomeUnavailable || x.Act() || x.Restricts() {
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
	if d, err := New(cfg("ext:github/intent", "decide", nil)); err == nil || !strings.Contains(err.Error(), `extension "github" is not enabled (enabled: none)`) {
		t.Errorf("point of an extension that isn't enabled: %v %v", d, err)
	}
	typo := cfg("test.acceprt", "observe", nil)
	typo.Points["test.acceprt"] = config.DecisionPoint{}
	if _, err := New(typo); err == nil || !strings.Contains(err.Error(), "no such point") {
		t.Errorf("disabled point with a typo'd id: err = %v", err)
	}
	off := cfg("ext:github/intent", "observe", nil)
	off.Points["ext:github/intent"] = config.DecisionPoint{}
	if d, err := New(off); d != nil || err != nil {
		t.Errorf("disabled point of an extension that isn't enabled: %v %v, want nil decider", d, err)
	}
}

func TestNewValidatesDeclaredExtensionPoints(t *testing.T) {
	intent := Point{ID: "ext:github/intent", Primary: "q", Questions: map[string]Question{"q": {Type: "noul"}}, Modes: []string{config.DecisionModeObserve}}
	cfg := func(id string, p config.DecisionPoint) config.DecisionsConfig {
		p.Handler = "p"
		return config.DecisionsConfig{Handlers: map[string]config.DecisionHandler{"p": {URL: "http://x"}}, Points: map[string]config.DecisionPoint{id: p}}
	}
	github := Extension{Name: "github", Points: []Point{intent}}
	if d, err := New(cfg(intent.ID, config.DecisionPoint{Enabled: true, Mode: "observe"}), github, Extension{Name: "sleeper"}); err != nil || !d.Enabled(intent.ID) {
		t.Errorf("declared point: %v %v", d, err)
	}
	for _, c := range []struct {
		name, id string
		p        config.DecisionPoint
		want     string
	}{
		{"typo'd name, disabled", "ext:github/intnet", config.DecisionPoint{}, `extension "github" declares no such point (declared: ext:github/intent)`},
		{"typo'd plugin, enabled", "ext:gihtub/intent", config.DecisionPoint{Enabled: true, Mode: "observe"}, `extension "gihtub" is not enabled (enabled: github, sleeper)`},
		{"extension declaring nothing", "ext:sleeper/start_sit", config.DecisionPoint{}, `extension "sleeper" declares no decision points`},
		{"unimplemented mode", intent.ID, config.DecisionPoint{Enabled: true, Mode: "decide"}, `mode "decide" is not supported`},
		{"fail closed, nothing restrictive", intent.ID, config.DecisionPoint{Enabled: true, Mode: "observe", Fail: config.DecisionFailClosed}, "restrictive answer"},
		{"unknown question", intent.ID, config.DecisionPoint{Enabled: true, Mode: "observe", Questions: map[string]config.DecisionQuestion{"zzz": {}}}, "no such question"},
	} {
		if _, err := New(cfg(c.id, c.p), github, Extension{Name: "sleeper"}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
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
	d.DecideWith(ctx, testAccept.Point, map[string]string{"plan": "p"}, "false")

	got := decisionEntries(t, mem, "chat-1")
	if len(got) != 1 {
		t.Fatalf("decision entries = %d, want 1", len(got))
	}
	p := got[0]
	if p.Point != "test.accept" || p.Mode != "decide" || p.Handler != "p" || p.Outcome != "act" || !p.Confident ||
		p.SkippedStep == nil || *p.SkippedStep != "test_judge" ||
		p.Top != "true" || p.TopP != 0.97 || p.Baseline != "" || p.InputTokens != 42 || p.ServerMS != 7 ||
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
	x := d.DecideWith(context.Background(), testAccept.Point, "s", "true")
	if x.Err == nil || x.Outcome != OutcomeRestrict || x.Top != "false" {
		t.Errorf("%+v, want a fail-closed restrict to false", x)
	}
	open := newDecider(t, "http://127.0.0.1:1", testAccept.ID, config.DecisionModeGuard, 0.9)
	if x := open.DecideWith(context.Background(), testAccept.Point, "s", "true"); x.Outcome != OutcomeUnavailable || x.Restricts() {
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

func TestClipEndsKeepsHeadAndTail(t *testing.T) {
	if got := ClipEnds("short", 10); got != "short" {
		t.Errorf("ClipEnds under the cap = %q", got)
	}
	long := "HEAD " + strings.Repeat("é", 500) + " TAIL"
	for _, n := range []int{40, 41, 42, 300} {
		got := ClipEnds(long, n)
		if len(got) > n || !utf8.ValidString(got) || !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, " TAIL") || !strings.Contains(got, "[truncated]") {
			t.Errorf("ClipEnds(_, %d) = %q (%d bytes)", n, got, len(got))
		}
	}
}

func TestFillBytesFollowsTheHandlerCap(t *testing.T) {
	d, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: "http://x", MaxInputTokens: 4096}},
		Points:   map[string]config.DecisionPoint{"test.observe_only": {Enabled: true, Handler: "clef", Mode: "observe"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.FillBytes("test.observe_only", 100, "abc", "de"); got != 4096*2-stateReserve-5 {
		t.Errorf("FillBytes = %d, want the cap's bytes less the reserve and used fields", got)
	}
	if got := d.FillBytes("test.observe_only", 20000); got != 20000 {
		t.Errorf("FillBytes = %d, want the floor", got)
	}
	if got := (*Decider)(nil).FillBytes("test.observe_only", 6000); got != 6000 {
		t.Errorf("disabled FillBytes = %d, want the floor", got)
	}
}

func TestClipBoundsBytesOnARuneBoundary(t *testing.T) {
	if got := Clip("short", 10); got != "short" {
		t.Errorf("Clip under the cap = %q", got)
	}
	long := strings.Repeat("é", 100) // 2 bytes each
	for _, n := range []int{0, 5, 20, 21, 199} {
		got := Clip(long, n)
		if len(got) > max(n, len(clipMarker)) || !strings.HasSuffix(got, clipMarker) || !utf8.ValidString(got) {
			t.Errorf("Clip(_, %d) = %q (%d bytes)", n, got, len(got))
		}
	}
}

func TestAnnotatedMetaIsRecordedButNeverSent(t *testing.T) {
	mem := ledgerCapture(t)
	var calls atomic.Int32
	bodies := make(chan map[string]any, 1)
	d := newDecider(t, noulServer(t, &calls, 0.2, bodies), testObserveOnly.ID, config.DecisionModeObserve, 0.9)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-meta"})
	<-d.Observe(ctx, testObserveOnly.Point, Annotated{State: map[string]string{"m": "x"}, Meta: map[string]bool{"prefiltered": true}})("false")

	if sent, _ := json.Marshal((<-bodies)["state"]); string(sent) != `{"m":"x"}` {
		t.Errorf("handler got state %s, want only the inner state", sent)
	}
	got := decisionEntries(t, mem, "chat-meta")
	if len(got) != 1 || string(got[0].State) != `{"m":"x"}` || string(got[0].Meta) != `{"prefiltered":true}` {
		t.Errorf("entries = %+v, want inner state and meta recorded apart", got)
	}
}

var testNoul = RegisterObserveNoul("test.noul", "q", "q?", "test_step")

func TestRegisterObserveNoulIsObserveOnly(t *testing.T) {
	_, err := New(config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"p": {URL: "http://x"}},
		Points:   map[string]config.DecisionPoint{testNoul.ID: {Enabled: true, Handler: "p", Mode: config.DecisionModeGuard}},
	})
	if err == nil || !testNoul.Parse("true") || testNoul.Parse("false") || testNoul.Replaces != "test_step" || testNoul.Questions["q"].Type != "noul" {
		t.Errorf("point = %+v, guard err = %v; want an observe-only noul point parsed as bool", testNoul.Point, err)
	}
}

// TestAwaitSettlesWithTheCallersBaseline: Await answers before the caller's step runs and records once
// settled. Acts gates which confident answers act; an act's baseline is recorded only as an audit.
func TestAwaitSettlesWithTheCallersBaseline(t *testing.T) {
	for _, c := range []struct {
		name, settle, outcome, baseline string
		p                               float64
		skipped, reason                 bool
	}{
		{"confident true acts, no audit", "", "act", "", 0.97, true, false},
		{"confident true acts, audited", "false", "act", "false", 0.97, true, false},
		{"confident false falls back", "true", "fallback", "true", 0.02, false, true},
		{"unsure falls back", "false", "fallback", "false", 0.6, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mem := ledgerCapture(t)
			var calls atomic.Int32
			d := newDecider(t, noulServer(t, &calls, c.p, nil), testActsTrue.ID, config.DecisionModeDecide, 0.9)
			ctx, cancel := context.WithCancel(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-aw"}))
			r, settle := d.Await(ctx, testActsTrue.Point, "s")
			if string(r.Outcome) != c.outcome || calls.Load() != 1 || len(decisionEntries(t, mem, "chat-aw")) != 0 {
				t.Fatalf("Await = %s after %d calls, want %s and nothing recorded before settle", r.Outcome, calls.Load(), c.outcome)
			}
			cancel() // an audit settles after the caller's ctx has ended
			<-settle(c.settle)
			got := decisionEntries(t, mem, "chat-aw")
			if len(got) != 1 || got[0].Outcome != c.outcome || got[0].Baseline != c.baseline ||
				(got[0].SkippedStep != nil) != c.skipped || (got[0].Reason != "") != c.reason {
				t.Errorf("records = %+v", got)
			}
		})
	}
}

func TestAuditDrawsAtTheConfiguredRate(t *testing.T) {
	var calls atomic.Int32
	url := noulServer(t, &calls, 0.9, nil)
	for _, c := range []struct {
		rate *float64
		want bool
	}{{nil, false}, {new(0.0), false}, {new(1.0), true}} {
		d, err := New(config.DecisionsConfig{
			Handlers: map[string]config.DecisionHandler{"p": {URL: url}},
			Points:   map[string]config.DecisionPoint{testActsTrue.ID: {Enabled: true, Handler: "p", Mode: config.DecisionModeDecide, AuditRate: c.rate}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Audit(testActsTrue.ID); got != c.want || d.Mode(testActsTrue.ID) != config.DecisionModeDecide {
			t.Errorf("Audit at %v = %v, want %v", c.rate, got, c.want)
		}
	}
	var none *Decider
	if none.Audit(testActsTrue.ID) || none.Mode(testActsTrue.ID) != "" {
		t.Error("a nil Decider audits nothing and has no mode")
	}
}
