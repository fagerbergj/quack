package tools

import (
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// PlanCache holds plans by ID so execute can reference them losslessly.
type PlanCache struct {
	mu             sync.Mutex
	plans          map[string]dag.Plan
	delivered      string
	selected       string
	rejected       bool
	rejectedReason string
}

// NewPlanCache returns an empty cache.
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

// RecordRejection notes that the plan judge declined a proposed plan this turn -
// distinguishes "planning was attempted and never accepted" (a failed run, #693)
// from "no plan was needed" (a normal direct answer).
func (c *PlanCache) RecordRejection(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejected = true
	c.rejectedReason = reason
}

// Rejected reports whether any plan-judge rejection occurred this turn, and its
// (last) reason - for the caller's own failure signaling, never for the reply.
func (c *PlanCache) Rejected() (reason string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectedReason, c.rejected
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
