package config

import (
	"strings"
	"testing"
	"time"
)

func TestDecisionsDefaults(t *testing.T) {
	t.Setenv("QUACK_TEST_CLEF_URL", "http://clef")
	c, err := Load(writeTemp(t, baseConfig+`
decisions:
  handlers:
    clef: { url: "${QUACK_TEST_CLEF_URL}" }
  points:
    plan.accept: { enabled: true, handler: clef, questions: { accept: { instructions: "ok?", criteria: { "true": yes } } } }
    ext:github/intent: { mode: guard, fail: closed, timeout: 1s }
    later: { handler: not-yet-defined }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := c.Decisions.Handlers["clef"]
	if h.Kind != "systemone" || h.URL != "http://clef" || h.Model != "clef" || h.Timeout != 5*time.Second || h.MaxInputTokens != 8192 {
		t.Errorf("handler defaults = %+v", h)
	}
	p := c.Decisions.Points["plan.accept"]
	if p.Mode != DecisionModeObserve || p.Fail != DecisionFailOpen || p.ActAt != 0.9 || p.Questions["accept"].Instructions != "ok?" {
		t.Errorf("point defaults = %+v", p)
	}
	if g := c.Decisions.Points["ext:github/intent"]; g.Fail != DecisionFailClosed || g.Timeout != time.Second {
		t.Errorf("ext point = %+v", g)
	}
	if c.Decisions.Points["later"].Enabled {
		t.Error("a point with no enabled key must default to off")
	}
}

func TestDecisionsAbsentIsOff(t *testing.T) {
	c, err := Load(writeTemp(t, baseConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Decisions.Points) != 0 || len(c.Decisions.Handlers) != 0 {
		t.Errorf("decisions = %+v, want empty", c.Decisions)
	}
}

func TestDecisionsValidation(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{"no url", "handlers: { h: { timeout: 1s } }", "decisions.handlers.h.url is required"},
		{"unknown kind", "handlers: { h: { kind: grpc, url: u } }", "kind must be systemone"},
		{"negative timeout", "handlers: { h: { url: u, timeout: -1s } }", "must be >= 0"},
		{"bad mode", "points: { p: { mode: shadow } }", "mode must be observe, guard or decide"},
		{"act_at above 1", "points: { p: { act_at: 1.5 } }", "act_at must be in (0,1]"},
		{"act_at negative", "points: { p: { act_at: -0.1 } }", "act_at must be in (0,1]"},
		{"negative point timeout", "points: { p: { timeout: -1s } }", "timeout >= 0"},
		{"fail closed outside guard", "points: { p: { mode: decide, fail: closed } }", "closed in guard mode"},
		{"bad fail", "points: { p: { mode: guard, fail: maybe } }", "fail must be open"},
		{"malformed ext id", `points: { "ext:github": { } }`, "ext:<plugin>/<name>"},
		{"enabled without handler", "points: { p: { enabled: true, handler: nope } }", `handler "nope" is not defined`},
	} {
		_, err := Load(writeTemp(t, baseConfig+"\ndecisions: { "+c.yaml+" }\n"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}
