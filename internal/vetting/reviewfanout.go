package vetting

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ReviewFanout: run-scoped accumulator for a plan with more than one
// reviewer node (#867). A review VERDICT is semantically run-scoped even
// though delivery used to be node-scoped: the first reviewer node to finish
// could post a real APPROVED review while siblings were still running.
// Reviewer nodes stage into this instead of delivering themselves; the last
// one to reach a terminal state merges everything staged so far and
// delivers exactly once.
type ReviewFanout struct {
	mu        sync.Mutex
	planID    string
	total     int
	terminal  map[string]reviewFanoutEntry
	delivered bool
}

type reviewFanoutEntry struct {
	item   StagedDelivery
	ok     bool
	failed bool // node errored/cancelled: contributes no verdict, noted in the merge
}

var reviewFanouts sync.Map // plan ID -> *ReviewFanout

// GetReviewFanout returns the shared fan-in for a plan, creating it on the
// first call. total is the plan's reviewer-node count. Only called for
// plans with more than one reviewer node - single-reviewer plans keep
// today's node-scoped delivery (cfg.ReviewFanout stays nil).
func GetReviewFanout(planID string, total int) *ReviewFanout {
	v, _ := reviewFanouts.LoadOrStore(planID, &ReviewFanout{planID: planID, total: total})
	return v.(*ReviewFanout)
}

// forget drops the registry entry once delivered, mirroring
// UnregisterMemSession - keeps the map from growing across a long process.
func (f *ReviewFanout) forget() {
	reviewFanouts.Delete(f.planID)
}

// SiblingsPending reports whether any reviewer node in the plan, besides
// whichever one is asking, is still running. Used by the staging seam
// (ReviewStage.SetVerdict) to refuse an early approve while an
// early request_changes is still allowed.
func (f *ReviewFanout) SiblingsPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// total - terminal counts every not-yet-terminal node, including the
	// caller itself (which hasn't reached its own terminal state yet since
	// it's still staging) - more than one means a sibling is also pending.
	return f.total-len(f.terminal) > 1
}

// Finish records nodeID's terminal outcome. item/ok is this node's own
// staged review (ok=false if it staged nothing, or aborted before staging).
// failed marks a node that errored or was cancelled, so it must not block
// the run forever waiting on it. Once every reviewer node in the plan has
// called Finish, the caller that completes the set gets deliver=true and
// the merged review - callers must deliver on that signal exactly once.
func (f *ReviewFanout) Finish(nodeID string, item StagedDelivery, ok, failed bool) (merged StagedDelivery, deliver bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminal == nil {
		f.terminal = map[string]reviewFanoutEntry{}
	}
	f.terminal[nodeID] = reviewFanoutEntry{item: item, ok: ok, failed: failed}
	if len(f.terminal) < f.total || f.delivered {
		return StagedDelivery{}, false
	}
	f.delivered = true
	return mergeReviews(f.terminal), true
}

// verdictRank: worst-of ordering - request_changes beats approve beats a
// plain comment.
var verdictRank = map[string]int{"comment": 0, "approve": 1, "request_changes": 2}

// mergeReviews: worst-of verdict, findings merged and attributed per node.
// A failed/cancelled sibling contributes no verdict but is named in the
// body rather than silently dropped - the point of this fix is that
// nothing gets swept under the rug.
func mergeReviews(terminal map[string]reviewFanoutEntry) StagedDelivery {
	ids := make([]string, 0, len(terminal))
	for id := range terminal {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	verdict := "comment"
	var sections []string
	var comments []ReviewComment
	var notes []string
	for _, id := range ids {
		e := terminal[id]
		if e.failed {
			notes = append(notes, fmt.Sprintf("- %s: did not complete, excluded from this verdict", id))
			continue
		}
		if !e.ok {
			notes = append(notes, fmt.Sprintf("- %s: completed without staging a review", id))
			continue
		}
		event := e.item.Event
		if event == "" {
			event = "comment"
		}
		if verdictRank[event] > verdictRank[verdict] {
			verdict = event
		}
		if strings.TrimSpace(e.item.Body) != "" {
			sections = append(sections, fmt.Sprintf("### %s\n%s", id, strings.TrimSpace(e.item.Body)))
		}
		for _, c := range e.item.Comments {
			c.Body = fmt.Sprintf("[%s] %s", id, c.Body)
			comments = append(comments, c)
		}
	}
	if len(notes) > 0 {
		sections = append(sections, "### Incomplete\n"+strings.Join(notes, "\n"))
	}
	return StagedDelivery{
		Kind:     "review",
		Event:    verdict,
		Body:     strings.Join(sections, "\n\n"),
		Comments: comments,
	}
}
