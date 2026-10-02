package decide

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// ErrDisabled: the point isn't enabled, so no call was made.
var ErrDisabled = errors.New("decide: point disabled")

// Outcome is what the point's policy did with a decision.
type Outcome string

const (
	OutcomeDisabled    Outcome = "disabled"
	OutcomeUnavailable Outcome = "unavailable" // no decision, fail open: service down, cold, timed out, over the cap
	OutcomeObserve     Outcome = "observe"
	OutcomeRestrict    Outcome = "restrict" // guard: confident restrictive answer, or no decision with fail: closed
	OutcomePass        Outcome = "pass"     // guard: anything else
	OutcomeAct         Outcome = "act"      // decide: confident, the caller's step is replaced
	OutcomeFallback    Outcome = "fallback" // decide: below act_at, the caller's own logic stands
)

// Result is one point evaluation. Err != nil means the handler gave no decision;
// the policy helpers then report false unless a guard fails closed.
type Result struct {
	Point, Mode, Handler string
	// Answers: question id -> option -> probability.
	Answers map[string]map[string]float64
	Top     string
	TopP    float64
	// Confident is TopP >= act_at whatever the mode: the counterfactual an observe run records.
	Confident    bool
	Outcome      Outcome
	RequestBytes int
	InputTokens  int
	ServerMS     float64
	Latency      time.Duration
	Err          error

	replaces  string
	state     any
	questions map[string]Question
	span      oteltrace.Span
}

// Act reports decide mode acting: the caller uses Top instead of its own step.
func (r Result) Act() bool { return r.Outcome == OutcomeAct }

// Restricts reports guard mode restricting: the caller narrows to Top.
func (r Result) Restricts() bool { return r.Outcome == OutcomeRestrict }

// SkippedStep is the step an acting decide outcome replaced, "" otherwise.
func (r Result) SkippedStep() string {
	if r.Act() {
		return r.replaces
	}
	return ""
}

// Decider holds the enabled points and their handlers. A nil *Decider is
// valid and has every point disabled.
type Decider struct {
	points   map[string]config.DecisionPoint
	handlers map[string]Handler
}

// New checks enabled points against the registry; nil when none is enabled. It never calls a handler.
func New(cfg config.DecisionsConfig) (*Decider, error) {
	d := &Decider{points: map[string]config.DecisionPoint{}, handlers: map[string]Handler{}}
	for id, p := range cfg.Points {
		if !p.Enabled {
			continue
		}
		if err := checkPoint(id, p); err != nil {
			return nil, err
		}
		d.points[id] = p
		if d.handlers[p.Handler] == nil {
			d.handlers[p.Handler] = newHandler(cfg.Handlers[p.Handler])
		}
	}
	if len(d.points) == 0 {
		return nil, nil
	}
	return d, nil
}

func checkPoint(id string, p config.DecisionPoint) error {
	if strings.HasPrefix(id, config.DecisionExtPrefix) {
		return nil
	}
	def, ok := lookup(id)
	if !ok {
		return fmt.Errorf("decisions.points.%s: no such point (registered: %s)", id, strings.Join(registeredIDs(), ", "))
	}
	if def.Modes != nil && !slices.Contains(def.Modes, p.Mode) {
		return fmt.Errorf("decisions.points.%s: mode %q is not supported here (supported: %s)", id, p.Mode, strings.Join(def.Modes, ", "))
	}
	if p.Fail == config.DecisionFailClosed && len(def.Restrictive) == 0 {
		return fmt.Errorf("decisions.points.%s: fail: closed needs a point with a restrictive answer", id)
	}
	for q := range p.Questions {
		if _, ok := def.Questions[q]; !ok {
			return fmt.Errorf("decisions.points.%s.questions.%s: the point has no such question", id, q)
		}
	}
	return nil
}

// Enabled reports whether pointID would make a call.
func (d *Decider) Enabled(pointID string) bool {
	if d == nil {
		return false
	}
	_, ok := d.points[pointID]
	return ok
}

// Decide evaluates a registered point and records it; baseline is what quack's own logic decided.
func (d *Decider) Decide(ctx context.Context, pointID string, state any, baseline string) Result {
	p, ok := lookup(pointID)
	if !ok {
		return Result{Point: pointID, Outcome: OutcomeDisabled, Err: ErrDisabled}
	}
	return d.DecideWith(ctx, p, state, baseline)
}

// DecideWith is Decide for a point defined at the call site (an extension's).
func (d *Decider) DecideWith(ctx context.Context, p Point, state any, baseline string) Result {
	if !d.Enabled(p.ID) {
		return Result{Point: p.ID, Outcome: OutcomeDisabled, Err: ErrDisabled}
	}
	r := d.run(ctx, p, state)
	record(ctx, r, baseline)
	return r
}

// Observe starts p now and returns settle, which records it against the caller's
// baseline once both are known. Neither blocks; call settle exactly once (it ends the span).
func (d *Decider) Observe(ctx context.Context, p Point, state any) (settle func(baseline string) <-chan struct{}) {
	if !d.Enabled(p.ID) {
		return func(string) <-chan struct{} { c := make(chan struct{}); close(c); return c }
	}
	ctx = context.WithoutCancel(ctx)
	pending := make(chan Result, 1)
	go func() {
		r := d.run(ctx, p, state)
		r.Outcome = OutcomeObserve
		if r.Err != nil {
			r.Outcome, r.Top = OutcomeUnavailable, ""
		}
		pending <- r
	}()
	return func(baseline string) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			record(ctx, <-pending, baseline)
		}()
		return done
	}
}

func (d *Decider) run(ctx context.Context, p Point, state any) Result {
	cfg := d.points[p.ID]
	r := Result{Point: p.ID, Mode: cfg.Mode, Handler: cfg.Handler, replaces: p.Replaces, state: state,
		questions: withOverrides(p.Questions, cfg.Questions)}
	ctx, r.span = otelobs.Start(ctx, "decision")
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	start := time.Now()
	reply, err := d.handlers[cfg.Handler].Ask(ctx, state, r.questions)
	r.Latency, r.RequestBytes = time.Since(start), reply.RequestBytes
	if err == nil && reply.Answers[p.Primary] == nil {
		err = fmt.Errorf("decide: %s: no answer for question %q", p.ID, p.Primary)
	}
	if err != nil {
		r.Err, r.Outcome = err, OutcomeUnavailable
		if cfg.Mode == config.DecisionModeGuard && cfg.Fail == config.DecisionFailClosed && len(p.Restrictive) > 0 {
			r.Outcome, r.Top = OutcomeRestrict, p.Restrictive[0]
		}
		return r
	}
	r.Answers, r.InputTokens, r.ServerMS = reply.Answers, reply.InputTokens, reply.ServerMS
	r.Top, r.TopP = top(reply.Answers[p.Primary])
	r.Confident = r.TopP >= cfg.ActAt
	r.Outcome = policy(cfg.Mode, r.Confident, slices.Contains(p.Restrictive, r.Top))
	return r
}

func policy(mode string, confident, restrictive bool) Outcome {
	switch mode {
	case config.DecisionModeGuard:
		if confident && restrictive {
			return OutcomeRestrict
		}
		return OutcomePass
	case config.DecisionModeDecide:
		if confident {
			return OutcomeAct
		}
		return OutcomeFallback
	}
	return OutcomeObserve
}

// top is the argmax; ties go to the lexically first option so a rerun records the same answer.
func top(probs map[string]float64) (string, float64) {
	var best string
	bestP := -1.0
	for _, opt := range slices.Sorted(maps.Keys(probs)) {
		if probs[opt] > bestP {
			best, bestP = opt, probs[opt]
		}
	}
	return best, bestP
}

func withOverrides(qs map[string]Question, overrides map[string]config.DecisionQuestion) map[string]Question {
	if len(overrides) == 0 {
		return qs
	}
	out := make(map[string]Question, len(qs))
	for id, q := range qs {
		if o, ok := overrides[id]; ok {
			if o.Instructions != "" {
				q.Instructions = o.Instructions
			}
			if o.Criteria != nil {
				q.Criteria = o.Criteria
			}
		}
		out[id] = q
	}
	return out
}
