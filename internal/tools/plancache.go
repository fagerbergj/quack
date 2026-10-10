package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// PlanCache: one per orchestrator turn, so a rejection recorded on it never outlives the turn.
type PlanCache struct {
	mu              sync.Mutex
	delivered       string
	selected        string
	rejectionCount  int
	rejectionReason string
	rejectedShapes  map[string]bool
	loopTripped     bool
	loopShape       string
}

// MaxPlanRejections caps plan-judge rejections per turn before the user is asked instead.
const MaxPlanRejections = 3

func NewPlanCache() *PlanCache {
	return &PlanCache{}
}

// SetDelivered records the terminal answer so the caller can persist it after the run.
func (c *PlanCache) SetDelivered(answer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delivered = answer
}

// Delivered returns the recorded answer (empty in synthesize mode).
func (c *PlanCache) Delivered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delivered
}

func (c *PlanCache) SetSelected(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selected = id
}

func (c *PlanCache) Selected() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selected, c.selected != ""
}

// RecordRejection: one rejection is normal iteration; tripped means this shape was already rejected
// this turn or the cap is reached, so re-planning is looping.
func (c *PlanCache) RecordRejection(reason, shape string) (tripped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejectionCount++
	c.rejectionReason = reason
	if c.rejectedShapes == nil {
		c.rejectedShapes = map[string]bool{}
	}
	if !c.loopTripped && (c.rejectedShapes[shape] || c.rejectionCount >= MaxPlanRejections) {
		c.loopTripped, c.loopShape = true, shape
	}
	c.rejectedShapes[shape] = true
	return c.loopTripped
}

// LoopGuard reports the plan loop guard tripped this turn, with the judge's latest reason and the
// ShapeKey of the plan it tripped on.
func (c *PlanCache) LoopGuard() (reason, shapeKey string, tripped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectionReason, ShapeKey(c.loopShape), c.loopTripped
}

// ShapeKey is a short, id-safe hash of a PlanShape.
func ShapeKey(shape string) string {
	sum := sha256.Sum256([]byte(shape))
	return hex.EncodeToString(sum[:6])
}

type waivedShapeKey struct{}

// WithWaivedPlanShape lets execute run the plan whose ShapeKey is key past the plan judge: the user
// chose to run that rejected plan as is. Any other plan is still judged.
func WithWaivedPlanShape(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, waivedShapeKey{}, key)
}

func waivedPlanShape(ctx context.Context) string {
	key, _ := ctx.Value(waivedShapeKey{}).(string)
	return key
}

// PlanShape fingerprints a plan by structure alone - each node's agent and its dependencies' agents,
// plus the delivery kind - so a re-plan that only renames ids or rewords tasks matches.
func PlanShape(nodes []dag.RawNode, delivery *dag.Delivery) string {
	agentOf := make(map[string]string, len(nodes))
	for _, n := range nodes {
		agentOf[n.ID] = n.Agent
	}
	lines := make([]string, 0, len(nodes))
	for _, n := range nodes {
		deps := make([]string, 0, len(n.DependsOn))
		for _, d := range n.DependsOn {
			deps = append(deps, agentOf[d])
		}
		slices.Sort(deps)
		lines = append(lines, n.Agent+"<"+strings.Join(deps, ","))
	}
	slices.Sort(lines)
	kind := ""
	if delivery != nil {
		kind = delivery.Kind
	}
	return kind + "|" + strings.Join(lines, ";")
}

// Rejections: this turn's count and latest reason, for failure signaling, never for the reply.
func (c *PlanCache) Rejections() (count int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectionCount, c.rejectionReason
}
