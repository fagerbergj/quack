package tools

import (
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// defaultPlanJudgeRoundCap bounds one turn's plan-judge loop: nothing else
// stops the model from retrying `plan` forever against a rejection.
const defaultPlanJudgeRoundCap = 5

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
	reasons         []string // distinct reasons in order, for the capped-turn failure message
	roundCap        int
}

func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan), roundCap: defaultPlanJudgeRoundCap}
}

// SetRoundCap overrides the plan-judge round cap; a non-positive value keeps the existing (default) cap.
func (c *PlanCache) SetRoundCap(roundCap int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if roundCap > 0 {
		c.roundCap = roundCap
	}
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

// RecordRejection notes that the plan judge declined a proposed plan this turn.
// A single rejection is normal iteration (the model may pivot to a direct
// answer, #760); repeated rejections exhaust the rejection budget (#693) - the Rejections count is how a caller tells the two apart, never the model's answer text.
func (c *PlanCache) RecordRejection(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejectionCount++
	c.rejectionReason = reason
	if len(c.reasons) == 0 || c.reasons[len(c.reasons)-1] != reason {
		c.reasons = append(c.reasons, reason)
	}
}

// Rejections returns how many times the plan judge rejected a proposed plan
// this turn, and the most recent reason - for the caller's own failure
// signaling, never for the reply.
func (c *PlanCache) Rejections() (count int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejectionCount, c.rejectionReason
}

// Capped reports whether the round cap has tripped, plus the distinct
// reasons seen so far for the failure message.
func (c *PlanCache) Capped() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejectionCount < c.roundCap {
		return false, ""
	}
	return true, strings.Join(c.reasons, "; ")
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
