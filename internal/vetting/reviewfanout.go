package vetting

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ReviewFanout: run-scoped accumulator for a plan with several reviewer nodes, so the first to finish can't post an
// APPROVED while siblings still run. Nodes stage here; the last to reach a terminal state merges and delivers once.
type ReviewFanout struct {
	mu        sync.Mutex
	planID    string
	total     int
	terminal  map[string]reviewFanoutEntry
	delivered bool

	// A downstream synthesizer owns the final consolidated review: delivery waits for it. On synthesizer
	// failure the merge falls back to per-node concatenation so nothing is stranded.
	synthWanted bool
	synthDone   bool
	synthBody   string // raw chat reply, last-resort fallback only (see mergeReviews)
	// The synthesizer's own code_review record fields, preferred over
	// parsing synthBody's free text when it wrote one (synthHaveRecord).
	synthHaveRecord bool
	synthVerdict    string
	synthTakeaway   string
	synthVerified   []string
	synthNotes      []string

	// cloneURL/branch: the repo a reviewer cloned. The delivering synthesizer never clones, so the first
	// reviewer to report one wins (they all review the same PR).
	cloneURL string
	branch   string

	// The PR's diff scope, resolved by whichever reviewer node reports it
	// first - every reviewer node in the fan-out reviews the same PR.
	scopeKnown       bool
	scopeHead        string
	scopeFiles       int
	scopeFirstReview bool
}

// RecordClone captures the repo/branch a reviewer node cloned, first one wins, so a delivering
// synthesizer with no clone of its own has coordinates to deliver against.
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

func (f *ReviewFanout) Clone() (cloneURL, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cloneURL, f.branch
}

// RecordScope captures the PR's diff scope, first reviewer node to resolve
// it wins - mirrors RecordClone, since every reviewer reviews the same PR.
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

// GetReviewFanout returns the shared fan-in for a plan, creating it on first call; total is the reviewer-node count.
// Only for multi-reviewer plans; single-reviewer plans deliver per node (cfg.ReviewFanout stays nil).
func GetReviewFanout(planID string, total int) *ReviewFanout {
	v, _ := reviewFanouts.LoadOrStore(planID, &ReviewFanout{planID: planID, total: total})
	return v.(*ReviewFanout)
}

// ResetReviewFanout drops a fan-in left over from a previous run: cleanup otherwise happens only in
// deliverMergedReview, so an interrupted run would poison every later run of the plan.
func ResetReviewFanout(planID string) {
	reviewFanouts.Delete(planID)
}

// forget drops the registry entry once delivered, mirroring
// UnregisterMemSession - keeps the map from growing across a long process.
func (f *ReviewFanout) forget() {
	reviewFanouts.Delete(f.planID)
}

// SiblingsPending reports whether any other reviewer node in the plan is still running, so
// ReviewStage.SetVerdict can refuse an early approve while still allowing an early request_changes.
func (f *ReviewFanout) SiblingsPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The caller itself is still staging, so it counts as not terminal: more than one means a sibling is pending.
	return f.total-len(f.terminal) > 1
}

// Finish records nodeID's terminal outcome; ok=false if it staged nothing, failed so an errored node can't block
// the run. The call completing the set gets deliver=true and the merged review, and must deliver exactly once.
func (f *ReviewFanout) Finish(nodeID string, item StagedDelivery, ok, failed bool) (merged StagedDelivery, deliver bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminal == nil {
		f.terminal = map[string]reviewFanoutEntry{}
	}
	f.terminal[nodeID] = reviewFanoutEntry{item: item, ok: ok, failed: failed}
	return f.deliverIfReady()
}

// SynthExpected reports whether this plan has a downstream synthesizer node:
// a reviewer node feeding one never owns the delivered verdict.
func (f *ReviewFanout) SynthExpected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synthWanted
}

// ExpectSynthesis marks the plan as having a downstream synthesizer, so delivery waits for
// FinishSynthesis. Called during graph assembly, before any node runs.
func (f *ReviewFanout) ExpectSynthesis() {
	f.mu.Lock()
	f.synthWanted = true
	f.mu.Unlock()
}

// FinishSynthesis records the synthesizer node's terminal outcome: rec's
// fields when it wrote a code_review record, else answer as a raw fallback.
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

// mergeReviews (mu held): the synthesizer's own code_review record wins
// when it wrote one, else its answer tail, else worst-of over the slices.
func (f *ReviewFanout) mergeReviews() StagedDelivery {
	ids := make([]string, 0, len(f.terminal))
	for id := range f.terminal {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// The synthesizer, once it produces output of its own, owns the
	// consolidated prose - a slice's own notes would just repeat it.
	synthOwnsProse := f.synthHaveRecord || f.synthBody != ""
	acc := reviewMergeAcc{}
	for _, id := range ids {
		acc.addSlice(id, f.terminal[id], synthOwnsProse)
	}
	acc.addSynth(f)

	// Still worst-of against a slice's verdict, never overwrites it outright:
	// an early slice request_changes must survive a later synthesizer approve.
	if f.synthVerdict != "" && (!acc.haveVerdict || verdictRank[f.synthVerdict] > verdictRank[acc.verdict]) {
		acc.verdict = f.synthVerdict
		acc.haveVerdict = true
	}
	if !acc.haveVerdict {
		acc.verdict = "comment"
	}

	takeaway, verified, notes := clampCodeReviewFields(acc.takeaway, acc.verified, acc.notes)
	body := renderReviewOverview(reviewOverviewInput{
		Verdict: acc.verdict, Takeaway: takeaway, Verified: verified, Notes: notes,
		Comments: acc.highlights, LegacySummary: acc.legacySummary,
		ScopeKnown: f.scopeKnown, HeadSHA: f.scopeHead, FileCount: f.scopeFiles, FirstReview: f.scopeFirstReview,
	})
	return StagedDelivery{
		Kind:     "review",
		Event:    acc.verdict,
		Body:     body,
		Comments: acc.ghComments,
	}
}

// reviewMergeAcc accumulates the merged review fields (verdict, prose, comments)
// across slices and the synthesizer in mergeReviews.
type reviewMergeAcc struct {
	verdict       string
	haveVerdict   bool
	takeaway      string
	legacySummary string
	legacyParts   []string
	verified      []string
	notes         []string
	highlights    []ReviewComment // for the renderer's label matching
	ghComments    []ReviewComment // for GitHub's inline posting, provenance kept off the wire
}

// addSlice folds one terminal slice's staged review into the accumulator.
func (a *reviewMergeAcc) addSlice(id string, e reviewFanoutEntry, synthOwnsProse bool) {
	if e.failed {
		a.notes = append(a.notes, fmt.Sprintf("%s: did not complete, excluded from this verdict", id))
		return
	}
	if !e.ok {
		a.notes = append(a.notes, fmt.Sprintf("%s: completed without staging a review", id))
		return
	}
	if event := e.item.Event; event != "" && (!a.haveVerdict || verdictRank[event] > verdictRank[a.verdict]) {
		a.verdict = event
		a.haveVerdict = true
	}
	if !synthOwnsProse {
		// Body (unlike Takeaway) isn't pre-capped, so it goes through the wider
		// legacy-summary bucket instead of the notes cap.
		if t := strings.TrimSpace(e.item.Takeaway); t != "" {
			a.notes = append(a.notes, fmt.Sprintf("%s: %s", id, t))
		} else if b := strings.TrimSpace(e.item.Body); b != "" {
			a.legacyParts = append(a.legacyParts, fmt.Sprintf("%s: %s", id, b))
		}
		for _, v := range e.item.Verified {
			a.verified = append(a.verified, fmt.Sprintf("%s: %s", id, v))
		}
		for _, n := range e.item.Notes {
			a.notes = append(a.notes, fmt.Sprintf("%s: %s", id, n))
		}
	}
	for _, c := range e.item.Comments {
		a.highlights = append(a.highlights, c)
		attributed := c
		attributed.SourceNode = id
		a.ghComments = append(a.ghComments, attributed)
	}
}

// addSynth folds the synthesizer's record (or parsed body, or raw fallback)
// into the accumulator, overriding slice prose where the synth owns it.
func (a *reviewMergeAcc) addSynth(f *ReviewFanout) {
	if len(a.legacyParts) > 0 {
		a.legacySummary = strings.Join(a.legacyParts, "\n")
	}
	switch {
	case f.synthHaveRecord:
		a.takeaway = f.synthTakeaway
		if len(f.synthVerified) > 0 {
			a.verified = f.synthVerified
		}
		if len(f.synthNotes) > 0 {
			a.notes = f.synthNotes
		}
	case f.synthBody != "":
		r := ParseAnswerReviewSections(f.synthBody)
		switch {
		case r.OK:
			if ev := r.Event; ev != "" && (!a.haveVerdict || verdictRank[ev] > verdictRank[a.verdict]) {
				a.verdict = ev
				a.haveVerdict = true
			}
			a.takeaway = r.Takeaway
			if len(r.Verified) > 0 {
				a.verified = r.Verified
			}
			if len(r.Notes) > 0 {
				a.notes = r.Notes
			}
		default:
			// No structured tail or record: last-resort fallback, the raw chat reply
			// folded in like a legacy summary (renderReviewOverview strips any staging narration).
			a.legacySummary = f.synthBody
		}
	}
}
