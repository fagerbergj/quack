package serve

import (
	"context"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/decide"
)

// extDecide mounts sdk.Host.Decide for one plugin, confined to its own
// ext:<plugin>/<name> points; any error is "no decision" to the extension.
func extDecide(d *decide.Decider, plugin string) func(context.Context, extsdk.DecideRequest) (extsdk.Decision, error) {
	return func(ctx context.Context, req extsdk.DecideRequest) (extsdk.Decision, error) {
		id, err := decide.ExtPointID(plugin, req.Point)
		if err != nil {
			return extsdk.Decision{}, err
		}
		qs := make(map[string]decide.Question, len(req.Questions))
		for k, q := range req.Questions {
			qs[k] = decide.Question{Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria}
		}
		r := d.DecideWith(ctx, decide.Point{ID: id, Questions: qs, Primary: req.Primary, Restrictive: req.Restrictive}, req.State, req.Baseline)
		if r.Err != nil {
			return extsdk.Decision{}, r.Err
		}
		return extsdk.Decision{
			Top: r.Top, TopP: r.TopP, Probabilities: r.Answers,
			Outcome: string(r.Outcome), Act: r.Act(), Restrict: r.Restricts(),
		}, nil
	}
}
