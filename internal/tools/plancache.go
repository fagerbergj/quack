package tools

import (
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// PlanCache holds plans produced by the plan tool so the execute tool can run
// them by ID. This avoids round-tripping the full plan JSON through the
// orchestrator model between the plan and execute calls — LLMs truncate or
// mangle large JSON when copying it from one tool result into the next tool
// call, which silently drops nodes (e.g. the synthesizer) from the executed
// plan. Passing a short ID instead is lossless.
//
// One cache is created per orchestrator run and shared by that run's plan and
// execute tools, so it never grows unbounded.
type PlanCache struct {
	mu    sync.Mutex
	plans map[string]dag.Plan
}

// NewPlanCache returns an empty cache.
func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan)}
}

// Put stores a plan keyed by its ID.
func (c *PlanCache) Put(p dag.Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[p.ID] = p
}

// Get returns the plan for id and whether it was found.
func (c *PlanCache) Get(id string) (dag.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[id]
	return p, ok
}
