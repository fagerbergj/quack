package vetting

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ReviewFanout: run-scoped accumulator for a plan with more than one reviewer node (#867). A review VERDICT is semantically run-scoped even though delivery used to be node-scoped: the first reviewer node to finish
// could post a real APPROVED review while siblings were still running. Reviewer nodes stage into this instead of delivering themselves; the last
// one to reach a terminal state merges everything staged so far and delivers exactly once.
type ReviewFanout struct {
	mu        sync.Mutex
	planID    string
	total     int
	terminal  map[string]reviewFanoutEntry
	delivered bool

	// A downstream synthesizer node owns the final consolidated review
	// (#965): delivery waits for it, and its answer becomes the summary
	// body. On synthesizer failure the merge falls back to the per-node concatenation so nothing is stranded.
	synthWanted bool
	synthDone   bool
	synthBody   string // raw chat reply, last-resort fallback only (see mergeReviews)
	// synthHaveRecord/synthVerdict/synthTakeaway/synthVerified/synthNotes:
	// the synthesizer's own code_review record, when it wrote one via
	// write_code_review - the same structured fields a single reviewer's
	// stage_review carries, preferred over parsing synthBody's free text.
	synthHaveRecord bool
	synthVerdict    string // authoritative over synthBody's tail (#1184)
	synthTakeaway   string
	synthVerified   []string
	synthNotes      []string

	// cloneURL/branch: the repo a reviewer node actually cloned (#1059). The
	// synthesizer node that ends up delivering the merged review never
	// clones anything itself, so it has no clone coordinates of its own - first reviewer to report one wins, the rest are the same repo/PR.
	cloneURL string
	branch   string

	// scopeKnown/scopeHead/scopeFiles/scopeFirstReview: the PR's diff scope
	// (Scope line, section 3), resolved by whichever reviewer node reports
	// it first - mirrors cloneURL/branch, since every reviewer node in the
	// fan-out reviews the same PR.
	scopeKnown       bool
	scopeHead        string
	scopeFiles       int
	scopeFirstReview bool
}

// RecordClone captures the repo/branch a reviewer node cloned, first one
// wins. Called before Finish/FinishSynthesis so the eventual deliverer -
// possibly a synthesizer node with no clone of its own - has coordinates to deliver against (#1059).
func (f *ReviewFanout) RecordClone(cloneURL, branch string) {
	if cloneURL == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cloneURL == "" {
		f.cloneURL, f.branch = cloneURL, branch
	}
}

// Clone returns the recorded reviewer clone coordinates, if any.
func (f *ReviewFanout) Clone() (cloneURL, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cloneURL, f.branch
}

// RecordScope captures the PR's diff scope (head sha, changed-file count,
// first-review-or-not) the first reviewer node to resolve it wins - every
// reviewer node in the fan-out reviews the same PR, so the first resolution
// is authoritative for the whole run, same pattern as RecordClone.
func (f *ReviewFanout) RecordScope(head string, files int, firstReview bool) {
	if head == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.scopeKnown {
		f.scopeKnown, f.scopeHead, f.scopeFiles, f.scopeFirstReview = true, head, files, firstReview
	}
}

type reviewFanoutEntry struct {
	item   StagedDelivery
	ok     bool
	failed bool // node errored/cancelled: contributes no verdict, noted in the merge
}

var reviewFanouts sync.Map // plan ID -> *ReviewFanout

// GetReviewFanout returns the shared fan-in for a plan, creating it on the
// first call. total is the plan's reviewer-node count. Only called for
// plans with more than one reviewer node - single-reviewer plans keep today's node-scoped delivery (cfg.ReviewFanout stays nil).
func GetReviewFanout(planID string, total int) *ReviewFanout {
	v, _ := reviewFanouts.LoadOrStore(planID, &ReviewFanout{planID: planID, total: total})
	return v.(*ReviewFanout)
}

// ResetReviewFanout drops any fan-in left over from a previous run of this
// plan. Cleanup otherwise happens on exactly one path (deliverMergedReview),
// so an interrupted run would poison every later run of the same plan (#1040).
func ResetReviewFanout(planID string) {
	reviewFanouts.Delete(planID)
}

// forget drops the registry entry once delivered, mirroring
// UnregisterMemSession - keeps the map from growing across a long process.
func (f *ReviewFanout) forget() {
	reviewFanouts.Delete(f.planID)
}

// SiblingsPending reports whether any reviewer node in the plan, besides
// whichever one is asking, is still running. Used by the staging seam
// (ReviewStage.SetVerdict) to refuse an early approve while an early request_changes is still allowed.
func (f *ReviewFanout) SiblingsPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// total - terminal counts every not-yet-terminal node, including the
	// caller itself (which hasn't reached its own terminal state yet since
	// it's still staging) - more than one means a sibling is also pending.
	return f.total-len(f.terminal) > 1
}

// Finish records nodeID's terminal outcome. item/ok is this node's own
// staged review (ok=false if it staged nothing, or aborted before staging). failed marks a node that errored or was cancelled, so it must not block
// the run forever waiting on it. Once every reviewer node in the plan has called Finish, the caller that completes the set gets deliver=true and the merged review - callers must deliver on that signal exactly once.
func (f *ReviewFanout) Finish(nodeID string, item StagedDelivery, ok, failed bool) (merged StagedDelivery, deliver bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminal == nil {
		f.terminal = map[string]reviewFanoutEntry{}
	}
	f.terminal[nodeID] = reviewFanoutEntry{item: item, ok: ok, failed: failed}
	return f.deliverIfReady()
}

// SynthExpected reports whether this plan has a downstream synthesizer node
// (#1092): a reviewer node feeding one never owns the delivered verdict.
func (f *ReviewFanout) SynthExpected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synthWanted
}

// ExpectSynthesis marks the plan as having a synthesizer node downstream of
// the reviewers: delivery waits for FinishSynthesis. Called during graph
// assembly, before any node runs.
func (f *ReviewFanout) ExpectSynthesis() {
	f.mu.Lock()
	f.synthWanted = true
	f.mu.Unlock()
}

// FinishSynthesis records the synthesizer node's terminal outcome. answer is
// its raw chat reply - a last-resort fallback (see mergeReviews) for when the
// synthesizer never wrote a code_review record at all. rec/haveRecord carry
// that record's structured takeaway/verified/notes/verdict when it exists -
// the same fields a single reviewer's stage_review call produces, and
// authoritative over answer's free text. Same exactly-once deliver contract as Finish.
func (f *ReviewFanout) FinishSynthesis(answer string, rec CodeReviewRecord, haveRecord bool) (merged StagedDelivery, deliver bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synthDone = true
	f.synthBody = strings.TrimSpace(answer)
	if haveRecord {
		f.synthHaveRecord = true
		f.synthVerdict = rec.Verdict
		f.synthTakeaway = rec.Takeaway
		f.synthVerified = rec.Verified
		f.synthNotes = rec.Notes
	}
	return f.deliverIfReady()
}

// deliverIfReady (mu held): all reviewers terminal, synthesizer too when one
// is expected, exactly once.
func (f *ReviewFanout) deliverIfReady() (StagedDelivery, bool) {
	if len(f.terminal) < f.total || (f.synthWanted && !f.synthDone) || f.delivered {
		return StagedDelivery{}, false
	}
	f.delivered = true
	return f.mergeReviews(), true
}

// verdictRank: worst-of ordering - request_changes beats approve beats a
// plain comment.
var verdictRank = map[string]int{"comment": 0, "approve": 1, "request_changes": 2}

// mergeReviews (mu held, called from deliverIfReady): the synthesizer's
// structured code_review record is authoritative when it wrote one
// (synthHaveRecord) - the same takeaway/verified/notes/verdict fields a
// single reviewer's stage_review produces, rendered through the same
// renderer. Absent that, this falls back to the synthesizer's answer-tail
// VERDICT, then to worst-of over slices that staged one (#867's defense: an
// early slice request_changes still beats a later synthesizer approve,
// since worst-of only ever raises the verdict, never lowers it). A slice
// with no event (V4: slices stage findings only, #1150) doesn't participate
// in worst-of at all - it used to default to "comment", the exact bug #1184
// reports. Findings are merged regardless of which verdict wins; a
// failed/cancelled sibling contributes no verdict but is named in the body
// rather than silently dropped.
func (f *ReviewFanout) mergeReviews() StagedDelivery {
	ids := make([]string, 0, len(f.terminal))
	for id := range f.terminal {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	verdict := "comment"
	haveVerdict := false
	// The synthesizer, once it produces output of its own, owns the
	// consolidated prose - a slice's own notes would just repeat it.
	synthOwnsProse := f.synthHaveRecord || f.synthBody != ""
	var highlights, ghComments []ReviewComment // highlights: for the renderer's label matching; ghComments: for GitHub's inline posting, provenance kept off the wire
	var verified, notes, legacyParts []string
	for _, id := range ids {
		e := f.terminal[id]
		if e.failed {
			notes = append(notes, fmt.Sprintf("%s: did not complete, excluded from this verdict", id))
			continue
		}
		if !e.ok {
			notes = append(notes, fmt.Sprintf("%s: completed without staging a review", id))
			continue
		}
		if event := e.item.Event; event != "" && (!haveVerdict || verdictRank[event] > verdictRank[verdict]) {
			verdict = event
			haveVerdict = true
		}
		if !synthOwnsProse {
			// Body (unlike Takeaway) isn't pre-capped, so it goes through
			// the wider legacy-summary bucket instead of the notes cap.
			if t := strings.TrimSpace(e.item.Takeaway); t != "" {
				notes = append(notes, fmt.Sprintf("%s: %s", id, t))
			} else if b := strings.TrimSpace(e.item.Body); b != "" {
				legacyParts = append(legacyParts, fmt.Sprintf("%s: %s", id, b))
			}
			for _, v := range e.item.Verified {
				verified = append(verified, fmt.Sprintf("%s: %s", id, v))
			}
			for _, n := range e.item.Notes {
				notes = append(notes, fmt.Sprintf("%s: %s", id, n))
			}
		}
		for _, c := range e.item.Comments {
			highlights = append(highlights, c)
			attributed := c
			attributed.SourceNode = id
			ghComments = append(ghComments, attributed)
		}
	}

	var takeaway, legacySummary string
	if len(legacyParts) > 0 {
		legacySummary = strings.Join(legacyParts, "\n")
	}
	switch {
	case f.synthHaveRecord:
		takeaway = f.synthTakeaway
		if len(f.synthVerified) > 0 {
			verified = f.synthVerified
		}
		if len(f.synthNotes) > 0 {
			notes = f.synthNotes
		}
	case f.synthBody != "":
		r := ParseAnswerReviewSections(f.synthBody)
		switch {
		case r.OK:
			if ev := r.Event; ev != "" && (!haveVerdict || verdictRank[ev] > verdictRank[verdict]) {
				verdict = ev
				haveVerdict = true
			}
			takeaway = r.Takeaway
			if len(r.Verified) > 0 {
				verified = r.Verified
			}
			if len(r.Notes) > 0 {
				notes = r.Notes
			}
		default:
			// No structured tail or record: last-resort fallback, the raw
			// chat reply folded in like a legacy summary (renderReviewOverview strips any staging narration from it).
			legacySummary = f.synthBody
		}
	}
	// The synthesizer's own structured code_review verdict is authoritative
	// over its answer-tail parse above, but still worst-of against a slice's
	// verdict rather than overwriting it outright - #867's defense (an early
	// slice request_changes must survive a later synthesizer approve) holds
	// regardless of which form the synthesizer's verdict took.
	if f.synthVerdict != "" && (!haveVerdict || verdictRank[f.synthVerdict] > verdictRank[verdict]) {
		verdict = f.synthVerdict
		haveVerdict = true
	}
	if !haveVerdict {
		verdict = "comment"
	}

	takeaway, verified, notes = clampCodeReviewFields(takeaway, verified, notes)
	body := renderReviewOverview(reviewOverviewInput{
		Verdict: verdict, Takeaway: takeaway, Verified: verified, Notes: notes,
		Comments: highlights, LegacySummary: legacySummary,
		ScopeKnown: f.scopeKnown, HeadSHA: f.scopeHead, FileCount: f.scopeFiles, FirstReview: f.scopeFirstReview,
	})
	return StagedDelivery{
		Kind:     "review",
		Event:    verdict,
		Body:     body,
		Comments: ghComments,
	}
}
