package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Decision point modes: observe records only, guard may only narrow what the
// caller does, decide replaces the caller's step when confident.
const (
	DecisionModeObserve = "observe"
	DecisionModeGuard   = "guard"
	DecisionModeDecide  = "decide"
)

// Decision fail modes: open falls back to quack's own logic when there is no
// decision; closed (guard only) restricts instead.
const (
	DecisionFailOpen   = "open"
	DecisionFailClosed = "closed"
)

// DecisionHandlerSystemOne is the one handler kind: a POST /v1/systemone endpoint (Clef/Kev/Jev).
const DecisionHandlerSystemOne = "systemone"

// DecisionExtPrefix starts an extension's point id: ext:<plugin>/<name>. Every other id is core.
const DecisionExtPrefix = "ext:"

var extPointID = regexp.MustCompile(`^ext:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidExtPointID reports whether id is a well-formed ext:<plugin>/<name>.
func ValidExtPointID(id string) bool { return extPointID.MatchString(id) }

const (
	defaultDecisionTimeout        = 5 * time.Second
	defaultDecisionMaxInputTokens = 8192
	defaultDecisionActAt          = 0.9
)

// DecisionsConfig is the decisions: block: the handlers that answer at
// intercept points, and the points themselves. Absent = every point off.
type DecisionsConfig struct {
	Handlers map[string]DecisionHandler `yaml:"handlers"`
	Points   map[string]DecisionPoint   `yaml:"points"`
}

// DecisionHandler is one handler instance; kind picks its implementation.
type DecisionHandler struct {
	Kind string `yaml:"kind"`
	URL  string `yaml:"url"`
	// Model is the request's model field; "" sends the handler's key.
	Model          string        `yaml:"model"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxInputTokens int           `yaml:"max_input_tokens"`
}

// DecisionPoint switches one intercept point on and sets its policy.
type DecisionPoint struct {
	Enabled bool   `yaml:"enabled"`
	Handler string `yaml:"handler"`
	Mode    string `yaml:"mode"`
	// ActAt is the top answer's minimum probability for guard/decide to act.
	ActAt float64 `yaml:"act_at"`
	// Timeout caps this point's call below the handler's; 0 keeps the handler's.
	Timeout   time.Duration               `yaml:"timeout"`
	Fail      string                      `yaml:"fail"`
	Questions map[string]DecisionQuestion `yaml:"questions"`
}

// DecisionQuestion overrides a point's built-in question text; the type stays the point's.
type DecisionQuestion struct {
	Instructions string `yaml:"instructions"`
	Criteria     any    `yaml:"criteria"`
}

func (c *Config) validateDecisions() error {
	d := &c.Decisions
	for name, h := range d.Handlers {
		if err := validateDecisionHandler(name, &h); err != nil {
			return err
		}
		d.Handlers[name] = h
	}
	for id, p := range d.Points {
		if err := d.validatePoint(id, &p); err != nil {
			return err
		}
		d.Points[id] = p
	}
	return nil
}

func validateDecisionHandler(name string, h *DecisionHandler) error {
	if h.Kind == "" {
		h.Kind = DecisionHandlerSystemOne
	}
	if h.Kind != DecisionHandlerSystemOne {
		return fmt.Errorf("config: decisions.handlers.%s.kind must be %s (got %q)", name, DecisionHandlerSystemOne, h.Kind)
	}
	if h.URL == "" {
		return fmt.Errorf("config: decisions.handlers.%s.url is required", name)
	}
	if h.Timeout < 0 || h.MaxInputTokens < 0 {
		return fmt.Errorf("config: decisions.handlers.%s: timeout and max_input_tokens must be >= 0", name)
	}
	if h.Model == "" {
		h.Model = name
	}
	if h.Timeout == 0 {
		h.Timeout = defaultDecisionTimeout
	}
	if h.MaxInputTokens == 0 {
		h.MaxInputTokens = defaultDecisionMaxInputTokens
	}
	return nil
}

func (d *DecisionsConfig) validatePoint(id string, p *DecisionPoint) error {
	if strings.HasPrefix(id, DecisionExtPrefix) && !ValidExtPointID(id) {
		return fmt.Errorf("config: decisions.points.%s: an extension point id is ext:<plugin>/<name>", id)
	}
	if err := validatePointPolicy(id, p); err != nil {
		return err
	}
	if p.ActAt == 0 {
		p.ActAt = defaultDecisionActAt
	}
	if p.ActAt <= 0 || p.ActAt > 1 || p.Timeout < 0 {
		return fmt.Errorf("config: decisions.points.%s: act_at must be in (0,1] and timeout >= 0", id)
	}
	if !p.Enabled {
		return nil
	}
	if _, ok := d.Handlers[p.Handler]; !ok {
		return fmt.Errorf("config: decisions.points.%s.handler %q is not defined under decisions.handlers", id, p.Handler)
	}
	return nil
}

func validatePointPolicy(id string, p *DecisionPoint) error {
	if p.Mode == "" {
		p.Mode = DecisionModeObserve
	}
	switch p.Mode {
	case DecisionModeObserve, DecisionModeGuard, DecisionModeDecide:
	default:
		return fmt.Errorf("config: decisions.points.%s.mode must be observe, guard or decide (got %q)", id, p.Mode)
	}
	if p.Fail == "" {
		p.Fail = DecisionFailOpen
	}
	if p.Fail != DecisionFailOpen && (p.Fail != DecisionFailClosed || p.Mode != DecisionModeGuard) {
		return fmt.Errorf("config: decisions.points.%s.fail must be open, or closed in guard mode (got %q)", id, p.Fail)
	}
	return nil
}
