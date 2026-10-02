package serve

import (
	"context"

	"github.com/fagerbergj/quack/internal/decide"
)

// extDecideFunc is the quack side of the proposed sdk.Host.Decide; the SDK has no
// such field yet, so nothing mounts it until quack-extensions adds one.
type extDecideFunc func(ctx context.Context, p decide.Point, state any, baseline string) (decide.Result, error)

// extDecide confines an extension to its own ext:<plugin>/<name> points; a point
// calls out only when decisions.points names it, and an error is always "no decision".
func extDecide(d *decide.Decider, plugin string) extDecideFunc {
	return func(ctx context.Context, p decide.Point, state any, baseline string) (decide.Result, error) {
		id, err := decide.ExtPointID(plugin, p.ID)
		if err != nil {
			return decide.Result{Point: p.ID, Outcome: decide.OutcomeDisabled, Err: err}, err
		}
		p.ID = id
		r := d.DecideWith(ctx, p, state, baseline)
		return r, r.Err
	}
}
