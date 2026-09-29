package tools

import (
	"slices"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// PlanCache holds plans by ID so execute can reference them losslessly. One
// instance per orchestrator turn (constructed fresh in Orchestrator.Run) - a
// rejection recorded on it never survives past that turn.
type PlanCache struct {
	mu              sync.Mutex
	plans           map[string]dag.Plan
	delivered       string
	selected        string
	rejectionCount  int
	rejectionReason string
	rejectedShapes  map[string]bool
	loopTripped     bool
}

// MaxPlanRejections caps plan-judge rejections per turn before the user is asked instead.
const MaxPlanRejections = 3

func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan)}
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

// Put stores a plan keyed by its ID.
func (c *PlanCache) SetSelected(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selected = id
}

// Selected returns the selected plan ID.
func (c *PlanCache) Selected() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selected, c.selected != ""
}

func (c *PlanCache) Put(p dag.Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[p.ID] = p
}

// Get returns the plan for id.
func (c *PlanCache) Get(id string) (dag.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[id]
	return p, ok
}

// RecordRejection notes a plan-judge rejection this turn (one is normal iteration, #760; more exhaust the budget, #693).
// tripped: shape (PlanShape) was already rejected this turn, or the cap is reached - re-planning is looping.
func (c *PlanCache) RecordRejection(reason, shape string) (tripped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejectionCount++
	c.rejectionReason = reason
	if c.rejectedShapes == nil {
		c.rejectedShapes = map[string]bool{}
	}
	c.loopTripped = c.loopTripped || c.rejectedShapes[shape] || c.rejectionCount >= MaxPlanRejections
	c.rejectedShapes[shape] = true
	return c.loopTripped
}

// LoopGuard reports the plan loop guard tripped this turn, with the judge's latest reason.
func (c *PlanCache) LoopGuard() (reason string, tripped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectionReason, c.loopTripped
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

// Rejections returns how many times the plan judge rejected a proposed plan
// this turn, and the most recent reason - for the caller's own failure
// signaling, never for the reply.
func (c *PlanCache) Rejections() (count int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectionCount, c.rejectionReason
}

// Pending reports whether a plan was created but never executed.
func (c *PlanCache) Pending() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selected != "" {
		return "", false
	}
	for id := range c.plans {
		return id, true
	}
	return "", false
}
