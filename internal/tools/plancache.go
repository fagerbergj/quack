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
	delivered string            // terminal answer when execute ran in deliver (end_turn) mode
	pending   *PendingInput     // the latest unresolved request_input pause this run (nil = none)
}

// PendingInput is the resume context for a node paused on request_input, snapshotted
// so the orchestrator's end-of-turn backstop can resume it deterministically if the
// model dropped it. It carries exactly what executor.Resume needs: the plan, the
// completed nodes' outputs (rehydrate downstream), the OTHER still-waiting nodes
// (kept blocked), and the target node + its open call ID.
type PendingInput struct {
	Plan    dag.Plan
	Done    map[string]string
	Waiting map[string]bool
	NodeID  string
	CallID  string
}

// NewPlanCache returns an empty cache.
func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan), results: make(map[string]string)}
}

// SetPending records an unresolved request_input pause; ClearPending drops it once
// the DAG is resolved. The orchestrator reads Pending after the model's turn: if
// non-nil (and the model didn't escalate to the user), the backstop resumes it.
func (c *PlanCache) SetPending(p *PendingInput) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = p
}

// ClearPending drops the pending input (the DAG ran to completion).
func (c *PlanCache) ClearPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = nil
}

// Pending returns the unresolved request_input pause, or nil.
func (c *PlanCache) Pending() *PendingInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
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
// user in deliver mode (end_turn=true). The orchestrator stays silent in that
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
