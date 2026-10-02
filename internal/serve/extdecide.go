package serve

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
)

// extDecisions backs every extension's Host.Decide. Boot fills it between building
// the extensions and starting them, since the declarations feed decide.New.
type extDecisions struct {
	decider *decide.Decider
	exts    []decide.Extension
	points  map[string]decide.Point // by ext:<plugin>/<name>
}

// declare records an enabled extension and its DecisionPoints in plugin's namespace; empty Modes is observe only.
func (x *extDecisions) declare(plugin string, ext extsdk.Extension) error {
	e := decide.Extension{Name: plugin}
	dp, _ := ext.(extsdk.DecisionPoints)
	for _, sp := range declarations(dp) {
		id, err := decide.ExtPointID(plugin, sp.Name)
		if err != nil {
			return fmt.Errorf("extensions.%s: decision point: %w", plugin, err)
		}
		if _, dup := x.points[id]; dup {
			return fmt.Errorf("extensions.%s: decision point %q declared twice", plugin, sp.Name)
		}
		if _, ok := sp.Questions[sp.Primary]; !ok {
			return fmt.Errorf("extensions.%s: decision point %q: primary question %q is not one of its questions", plugin, sp.Name, sp.Primary)
		}
		p := decide.Point{ID: id, Questions: decideQuestions(sp.Questions), Primary: sp.Primary, Restrictive: sp.Restrictive, Modes: sp.Modes}
		if len(p.Modes) == 0 {
			p.Modes = []string{config.DecisionModeObserve}
		}
		if x.points == nil {
			x.points = map[string]decide.Point{}
		}
		x.points[id] = p
		e.Points = append(e.Points, p)
	}
	x.exts = append(x.exts, e)
	return nil
}

func declarations(dp extsdk.DecisionPoints) []extsdk.DecisionPoint {
	if dp == nil {
		return nil
	}
	return dp.DecisionPoints()
}

// build validates cfg against the core and declared points, as boot does, and serves Decide from the result.
func (x *extDecisions) build(cfg config.DecisionsConfig) (*decide.Decider, error) {
	d, err := decide.New(cfg, x.exts...)
	x.decider = d
	return d, err
}

func decideQuestions(qs map[string]extsdk.DecisionQuestion) map[string]decide.Question {
	out := make(map[string]decide.Question, len(qs))
	for k, q := range qs {
		out[k] = decide.Question{Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria}
	}
	return out
}

// host mounts sdk.Host.Decide for one plugin, confined to its own declared
// points; any error is "no decision" to the extension.
func (x *extDecisions) host(plugin string) func(context.Context, extsdk.DecideRequest) (extsdk.Decision, error) {
	return func(ctx context.Context, req extsdk.DecideRequest) (extsdk.Decision, error) {
		id, err := decide.ExtPointID(plugin, req.Point)
		if err != nil {
			return extsdk.Decision{}, err
		}
		p, ok := x.points[id]
		if !ok {
			return extsdk.Decision{}, fmt.Errorf("%w: %s is not among %s's DecisionPoints", decide.ErrDisabled, id, plugin)
		}
		if !inlineMatches(p, req) {
			return extsdk.Decision{}, fmt.Errorf("decide: %s: the request's questions, primary or restrictive answers differ from the declared point", id)
		}
		r := x.decider.DecideWith(ctx, p, req.State, req.Baseline)
		if r.Err != nil {
			return extsdk.Decision{}, r.Err
		}
		return extsdk.Decision{
			Top: r.Top, TopP: r.TopP, Probabilities: r.Answers,
			Outcome: string(r.Outcome), Act: r.Act(), Restrict: r.Restricts(),
		}, nil
	}
}

// inlineMatches: a request may still carry the point's definition, but only the declared one.
func inlineMatches(p decide.Point, req extsdk.DecideRequest) bool {
	return (req.Questions == nil || reflect.DeepEqual(decideQuestions(req.Questions), p.Questions)) &&
		(req.Primary == "" || req.Primary == p.Primary) &&
		(req.Restrictive == nil || slices.Equal(req.Restrictive, p.Restrictive))
}
