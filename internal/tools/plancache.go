package tools

import (
	"regexp"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/dag"
)

// defaultPlanJudgeRoundCap/defaultPlanRejectionRepeatCap bound one turn's
// plan-judge loop - the QA rig hit 56 rejections over 347s with no cap.
const (
	defaultPlanJudgeRoundCap      = 5
	defaultPlanRejectionRepeatCap = 3
)

// reasonSimilarityThreshold: the plan judge is free text with no structured
// criteria to compare (vetting.PlanJudge returns only accept/reason/err), and
// on the QA rig's real rejections it reworded the same complaint every round
// rather than repeating verbatim - consecutive real rounds topped out around
// 0.6 Jaccard, never near a stricter bar, so exact string equality never
// caught a real streak and this threshold is tuned to that evidence.
const reasonSimilarityThreshold = 0.4

var reasonTokenRe = regexp.MustCompile(`[a-z0-9]+`)

// similarReason reports whether a and b are the same rejection complaint
// reworded, via Jaccard similarity over lowercased word tokens.
func similarReason(a, b string) bool {
	ta, tb := reasonTokenSet(a), reasonTokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return a == b
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return float64(inter)/float64(union) >= reasonSimilarityThreshold
}

func reasonTokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range reasonTokenRe.FindAllString(strings.ToLower(s), -1) {
		out[t] = true
	}
	return out
}

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
	reasonStreak    int      // consecutive rejections carrying the identical reason text
	reasons         []string // distinct reasons in order, for the capped-turn failure message
	roundCap        int
	repeatCap       int
}

func NewPlanCache() *PlanCache {
	return &PlanCache{plans: make(map[string]dag.Plan), roundCap: defaultPlanJudgeRoundCap, repeatCap: defaultPlanRejectionRepeatCap}
}

// SetCaps overrides the plan-judge round cap and repeated-reason cap; a
// non-positive value keeps the existing (default) cap.
func (c *PlanCache) SetCaps(roundCap, repeatCap int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if roundCap > 0 {
		c.roundCap = roundCap
	}
	if repeatCap > 0 {
		c.repeatCap = repeatCap
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
	if similarReason(reason, c.rejectionReason) {
		c.reasonStreak++
	} else {
		c.reasonStreak = 1
	}
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

// Capped reports whether the round cap or the repeated-reason cap has
// tripped, plus the distinct reasons seen so far for the failure message.
func (c *PlanCache) Capped() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejectionCount < c.roundCap && c.reasonStreak < c.repeatCap {
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
