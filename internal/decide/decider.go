package decide

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
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
	// Reason says why a confident decide answer fell back.
	Reason string

	audited   bool // an act whose replaced step still ran, so its baseline is real
	replaces  string
	state     any
	meta      any
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
	tokens   map[string]int // handler name -> max_input_tokens
}

// Extension is one enabled extension and the points it declared, ids already ext:<Name>/<name>.
type Extension struct {
	Name   string
	Points []Point
}

// New checks every point id against the registry and the enabled extensions'
// declared points (so a typo is caught while disabled), and enabled points' policy;
// nil when none is enabled. It never calls a handler.
func New(cfg config.DecisionsConfig, exts ...Extension) (*Decider, error) {
	d := &Decider{points: map[string]config.DecisionPoint{}, handlers: map[string]Handler{}, tokens: map[string]int{}}
	declared, names := map[string]Point{}, map[string][]string{}
	for _, e := range exts {
		names[e.Name] = []string{}
		for _, p := range e.Points {
			declared[p.ID] = p
			names[e.Name] = append(names[e.Name], p.ID)
		}
	}
	for id, p := range cfg.Points {
		def, ok := lookup(id)
		if !ok {
			def, ok = declared[id]
		}
		if !ok {
			if err := unknownPoint(id, p.Enabled, names); err != nil {
				return nil, err
			}
			continue
		}
		if !p.Enabled {
			continue
		}
		if err := checkPoint(id, p, def); err != nil {
			return nil, err
		}
		d.points[id] = p
		if d.handlers[p.Handler] == nil {
			d.handlers[p.Handler] = newHandler(cfg.Handlers[p.Handler])
			d.tokens[p.Handler] = cfg.Handlers[p.Handler].MaxInputTokens
		}
	}
	if len(d.points) == 0 {
		return nil, nil
	}
	return d, nil
}

// unknownPoint says why id resolves to no point; nil for a disabled entry of an
// extension that isn't enabled, which is inert and can't be told from a typo.
func unknownPoint(id string, enabled bool, exts map[string][]string) error {
	if !strings.HasPrefix(id, config.DecisionExtPrefix) {
		return fmt.Errorf("decisions.points.%s: no such point (registered: %s)", id, strings.Join(registeredIDs(), ", "))
	}
	plugin, _, _ := strings.Cut(strings.TrimPrefix(id, config.DecisionExtPrefix), "/")
	ids, on := exts[plugin]
	switch {
	case !on && !enabled:
		return nil
	case !on:
		return fmt.Errorf("decisions.points.%s: extension %q is not enabled (enabled: %s)", id, plugin, cmp.Or(strings.Join(slices.Sorted(maps.Keys(exts)), ", "), "none"))
	case len(ids) == 0:
		return fmt.Errorf("decisions.points.%s: extension %q declares no decision points", id, plugin)
	}
	return fmt.Errorf("decisions.points.%s: extension %q declares no such point (declared: %s)", id, plugin, strings.Join(slices.Sorted(slices.Values(ids)), ", "))
}

func checkPoint(id string, p config.DecisionPoint, def Point) error {
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

// Mode is pointID's configured mode, "" when it is disabled.
func (d *Decider) Mode(pointID string) string {
	if !d.Enabled(pointID) {
		return ""
	}
	return d.points[pointID].Mode
}

// Audit draws whether this act outcome of pointID still runs its replaced step, at the point's audit_rate.
func (d *Decider) Audit(pointID string) bool {
	if !d.Enabled(pointID) {
		return false
	}
	r := d.points[pointID].AuditRate
	return r != nil && rand.Float64() < *r
}

// Enabled reports whether pointID would make a call.
func (d *Decider) Enabled(pointID string) bool {
	if d == nil {
		return false
	}
	_, ok := d.points[pointID]
	return ok
}

// stateReserve is the request's bytes outside a state's fields: the questions and the JSON envelope.
const stateReserve = 1024

// FillBytes is what pointID's handler cap leaves for one state field after the used fields, at 2 bytes
// per token (web text ran to 2.8 under JSON escaping, so 3 overflowed an 8192 cap), never under floor.
func (d *Decider) FillBytes(pointID string, floor int, used ...string) int {
	if !d.Enabled(pointID) {
		return floor
	}
	n := d.tokens[d.points[pointID].Handler]*2 - stateReserve
	for _, u := range used {
		n -= len(u)
	}
	return max(floor, n)
}

// Decide evaluates a registered point and records it; baseline is what quack's own logic decided.
func (d *Decider) Decide(ctx context.Context, pointID string, state any, baseline string) Result {
	p, ok := lookup(pointID)
	if !ok {
		return Result{Point: pointID, Outcome: OutcomeDisabled, Err: ErrDisabled}
	}
	return d.DecideWith(ctx, p, state, baseline)
}

// DecideWith is Decide for a point New validated, core or an extension's declared one.
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
		return observeNothing
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

// Await is DecideWith that records later: it blocks for p's answer and returns settle, which records it
// once the caller's baseline is known. On an act, a non-empty baseline is an audit of the skipped step.
func (d *Decider) Await(ctx context.Context, p Point, state any) (Result, func(baseline string) <-chan struct{}) {
	if !d.Enabled(p.ID) {
		return Result{Point: p.ID, Outcome: OutcomeDisabled, Err: ErrDisabled}, observeNothing
	}
	ctx = context.WithoutCancel(ctx)
	r := d.run(ctx, p, state)
	return r, func(baseline string) <-chan struct{} {
		r.audited = r.Act() && baseline != ""
		record(ctx, r, baseline)
		return observeNothing(baseline)
	}
}

func observeNothing(string) <-chan struct{} { c := make(chan struct{}); close(c); return c }

func (d *Decider) run(ctx context.Context, p Point, state any) Result {
	cfg := d.points[p.ID]
	r := Result{Point: p.ID, Mode: cfg.Mode, Handler: cfg.Handler, replaces: p.Replaces, state: state,
		questions: withOverrides(p.Questions, cfg.Questions)}
	if a, ok := state.(Annotated); ok {
		r.state, r.meta = a.State, a.Meta
	}
	ctx, r.span = otelobs.Start(ctx, "decision")
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	start := time.Now()
	reply, err := d.handlers[cfg.Handler].Ask(ctx, r.state, r.questions)
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
	if r.Act() && p.Acts != nil && !slices.Contains(p.Acts, r.Top) {
		r.Outcome, r.Reason = OutcomeFallback, fmt.Sprintf("confident %q: %s acts only on %s", r.Top, p.ID, strings.Join(p.Acts, ", "))
	}
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
