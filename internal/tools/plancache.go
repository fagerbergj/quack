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
	mu        sync.Mutex
	plans     map[string]dag.Plan
	results   map[string]string // plan ID → terminal answer, memoised after first execute
	delivered string            // terminal answer the execute node delivered straight to the user
	selected  string            // plan ID the execute tool committed to this run
	lastID    string            // most-recently-Put plan (for the single-runner router)
}

// NewPlanCache returns an empty cache.
func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan), results: make(map[string]string)}
}

// SetResult memoises a plan's terminal answer so a repeat execute of the same
// plan_id returns the cached answer instead of re-running the whole DAG (the
// model sometimes calls execute twice — e.g. once to read the result, then again
// with end_turn=true). Re-execution would burn minutes and tokens redundantly.
func (c *PlanCache) SetResult(id, answer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[id] = answer
}

// Result returns the memoised answer for a plan and whether it was found.
func (c *PlanCache) Result(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.results[id]
	return a, ok
}

// SetDelivered records the terminal answer that execute streamed straight to the
// user in deliver mode. The orchestrator stays silent in that
// mode, so its session would otherwise hold no record of the answer; the caller
// reads this after the run to persist it as the turn's assistant message (fixing
// reload and follow-up conversation history).
func (c *PlanCache) SetDelivered(answer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delivered = answer
}

// Delivered returns the answer recorded by SetDelivered (empty if execute ran in
// synthesize mode, where the orchestrator authors the answer itself).
func (c *PlanCache) Delivered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delivered
}

// Put stores a plan keyed by its ID.
// SetSelected records the plan the execute tool committed to this run; the
// orchestrator runs it as a native graph after the llmagent's turn ends.
func (c *PlanCache) SetSelected(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selected = id
}

// Selected returns the plan ID the execute tool selected this run, if any.
func (c *PlanCache) Selected() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selected, c.selected != ""
}

func (c *PlanCache) Put(p dag.Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[p.ID] = p
	c.lastID = p.ID
}

// Latest returns the most-recently-Put plan (the single-runner router builds its
// gate-node stream from it once the plan tool has run).
func (c *PlanCache) Latest() (dag.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[c.lastID]
	return p, ok
}

// Get returns the plan for id and whether it was found.
func (c *PlanCache) Get(id string) (dag.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[id]
	return p, ok
}
