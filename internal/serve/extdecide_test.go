package serve

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

	ghext "github.com/fagerbergj/quack-extensions/github"
	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/otelobs"
)

type declaringExt struct {
	extsdk.Extension
	points []extsdk.DecisionPoint
}

func (e declaringExt) DecisionPoints() []extsdk.DecisionPoint { return e.points }

var intentPoint = extsdk.DecisionPoint{Name: "intent", Primary: "write", Restrictive: []string{"false"},
	Questions: map[string]extsdk.DecisionQuestion{"write": {Type: "noul", Instructions: "write?"}}, Modes: []string{"observe", "decide"}}

func declared(t *testing.T, plugin string, points ...extsdk.DecisionPoint) *extDecisions {
	t.Helper()
	x := &extDecisions{}
	if err := x.declare(plugin, declaringExt{points: points}); err != nil {
		t.Fatal(err)
	}
	return x
}

func decisionsCfg(url string, points map[string]config.DecisionPoint) config.DecisionsConfig {
	for id, p := range points {
		p.Handler, p.ActAt, p.Fail = "clef", 0.9, "open"
		points[id] = p
	}
	return config.DecisionsConfig{Handlers: map[string]config.DecisionHandler{"clef": {URL: url, Timeout: time.Second}}, Points: points}
}

func TestExtDecideServesOnlyDeclaredPoints(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"write": map[string]any{"noul": 0.97}}})
	}))
	defer srv.Close()
	x := declared(t, "github", intentPoint)
	if err := x.declare("sleeper", declaringExt{points: []extsdk.DecisionPoint{intentPoint}}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.build(decisionsCfg(srv.URL, map[string]config.DecisionPoint{
		"ext:github/intent":  {Enabled: true, Mode: "decide"},
		"ext:sleeper/intent": {Enabled: true, Mode: "decide"},
	})); err != nil {
		t.Fatal(err)
	}
	github := x.host("github")
	slim := extsdk.DecideRequest{Point: "intent", State: "push it now", Baseline: "false"}

	r, err := github(context.Background(), slim)
	if err != nil || !r.Act || r.Top != "true" || r.Outcome != "act" || r.Probabilities["write"]["true"] != 0.97 {
		t.Errorf("slim request: %+v %v, want ext:github/intent acting on true", r, err)
	}
	full := slim
	full.Questions, full.Primary, full.Restrictive = intentPoint.Questions, "write", []string{"false"}
	if _, err := github(context.Background(), full); err != nil {
		t.Errorf("inline definition equal to the declaration: %v", err)
	}
	for name, mutate := range map[string]func(*extsdk.DecideRequest){
		"questions": func(r *extsdk.DecideRequest) {
			r.Questions = map[string]extsdk.DecisionQuestion{"write": {Type: "noul"}}
		},
		"primary":     func(r *extsdk.DecideRequest) { r.Primary = "other" },
		"restrictive": func(r *extsdk.DecideRequest) { r.Restrictive = []string{"true"} },
	} {
		bad := slim
		mutate(&bad)
		if _, err := github(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "differ from the declared point") {
			t.Errorf("mismatched inline %s: err = %v", name, err)
		}
	}
	if _, err := github(context.Background(), extsdk.DecideRequest{Point: "review"}); !errors.Is(err, decide.ErrDisabled) || !strings.Contains(err.Error(), "not among github's DecisionPoints") {
		t.Errorf("undeclared point: err = %v, want ErrDisabled naming the declarations", err)
	}
	if _, err := github(context.Background(), extsdk.DecideRequest{Point: "ext:sleeper/intent"}); !errors.Is(err, decide.ErrNamespace) {
		t.Errorf("another extension's point: err = %v, want ErrNamespace", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want only the two matching requests to call out", calls.Load())
	}
}

func TestExtDecideNoEnabledPointIsDisabled(t *testing.T) {
	x := declared(t, "github", intentPoint)
	if d, err := x.build(config.DecisionsConfig{}); d != nil || err != nil {
		t.Fatalf("build with nothing enabled: %v %v", d, err)
	}
	if _, err := x.host("github")(context.Background(), extsdk.DecideRequest{Point: "intent"}); !errors.Is(err, decide.ErrDisabled) {
		t.Errorf("nil decider: err = %v, want ErrDisabled", err)
	}
}

func TestBootRejectsExtPointsTheExtensionDoesNotDeclare(t *testing.T) {
	observeOnly := intentPoint
	observeOnly.Modes = nil
	for _, c := range []struct {
		name, id, mode, want string
	}{
		{"typo'd name", "ext:github/intnet", "observe", `extension "github" declares no such point (declared: ext:github/intent)`},
		{"typo'd plugin", "ext:gihtub/intent", "observe", `extension "gihtub" is not enabled (enabled: github)`},
		{"unimplemented mode", "ext:github/intent", "decide", `mode "decide" is not supported`},
	} {
		x := declared(t, "github", observeOnly)
		if _, err := x.build(decisionsCfg("http://x", map[string]config.DecisionPoint{c.id: {Enabled: true, Mode: c.mode}})); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestDeclareRejectsMalformedPoints(t *testing.T) {
	noPrimary := intentPoint
	noPrimary.Primary = "missing"
	for name, points := range map[string][]extsdk.DecisionPoint{
		"bad name":        {{Name: "a/b"}},
		"duplicate":       {intentPoint, intentPoint},
		"primary missing": {noPrimary},
	} {
		if err := (&extDecisions{}).declare("github", declaringExt{points: points}); err == nil {
			t.Errorf("%s: declare accepted it", name)
		}
	}
	if err := (&extDecisions{}).declare("github", declaringExt{}); err != nil {
		t.Errorf("an extension declaring nothing: %v", err)
	}
}

// TestGitHubDeclaresTheDocumentedPoints: the ids and modes docs/configuration/decisions.md lists boot.
func TestGitHubDeclaresTheDocumentedPoints(t *testing.T) {
	x := &extDecisions{}
	if err := x.declare("github", &ghext.Extension{}); err != nil {
		t.Fatal(err)
	}
	points := map[string]config.DecisionPoint{}
	for _, id := range []string{"ext:github/finding.severity", "ext:github/finding.blocking", "ext:github/review.verdict"} {
		points[id] = config.DecisionPoint{Enabled: true, Mode: "observe"}
	}
	if d, err := x.build(decisionsCfg("http://x", points)); err != nil || !d.Enabled("ext:github/review.verdict") {
		t.Errorf("documented github points: %v %v", d, err)
	}
}

func TestExtDecideAttributesTheRequestChat(t *testing.T) {
	mem := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(mem))))
	t.Cleanup(otelobs.SetLoggerProviderForTesting(lp))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"write": map[string]any{"noul": 0.8}}})
	}))
	defer srv.Close()
	x := declared(t, "sleeper", intentPoint)
	if _, err := x.build(decisionsCfg(srv.URL, map[string]config.DecisionPoint{"ext:sleeper/intent": {Enabled: true, Mode: "observe"}})); err != nil {
		t.Fatal(err)
	}
	sleeper := x.host("sleeper")
	entries := func(chat string) []ledger.Entry {
		t.Helper()
		var out []ledger.Entry
		got, err := mem.ReadEntries(context.Background(), chat, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			if e.Kind == ledger.KindDecision {
				out = append(out, e)
			}
		}
		return out
	}

	chat := "ext:sleeper:1356:3:lineup"
	if _, err := sleeper(context.Background(), extsdk.DecideRequest{Point: "intent", Baseline: "true", ChatID: chat}); err != nil {
		t.Fatal(err)
	}
	if got := entries(chat); len(got) != 1 || got[0].NodeID != "" {
		t.Errorf("run-less request naming its chat: %d ledger entries %+v, want 1 with no node", len(got), got)
	}

	inRun := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chat, Node: "n1"})
	if _, err := sleeper(inRun, extsdk.DecideRequest{Point: "intent", Baseline: "true", ChatID: chat}); err != nil {
		t.Fatal(err)
	}
	if got := entries(chat); len(got) != 2 || got[1].NodeID != "n1" {
		t.Errorf("same chat as the ctx: entries %+v, want the ctx's node kept", got)
	}

	if _, err := sleeper(context.Background(), extsdk.DecideRequest{Point: "intent", Baseline: "true", ChatID: "ext:github:o-r-1"}); !errors.Is(err, decide.ErrNamespace) {
		t.Errorf("another plugin's chat: err = %v, want ErrNamespace", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want the rejected chat never to call out", calls.Load())
	}

	ctxChat := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-9"})
	if _, err := sleeper(ctxChat, extsdk.DecideRequest{Point: "intent", Baseline: "true"}); err != nil {
		t.Fatal(err)
	}
	if got := entries("chat-9"); len(got) != 1 {
		t.Errorf("no ChatID: %d entries under the ctx's chat, want 1", len(got))
	}
}
