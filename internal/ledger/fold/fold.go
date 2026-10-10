// Package fold projects a chat's ledger.Entry stream into read models (artifact revisions, node lifecycle,
// judge rounds): the one definition of the aborted/later-entry-wins rule.
package fold

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// pageSize bounds one page when the store supports paging. ponytail: fixed size, no adaptive backoff.
// A var so tests can shrink it to exercise multi-page reads.
var pageSize = 1000

// judgeRoundKind is vetting's kindJudgeRound artifact kind name, duplicated
// here (not imported) to keep fold dependency-free of vetting.
const judgeRoundKind = "judge_round"

// pagedReader is a LedgerStore that pages server-side (PGStore); others are read in one ReadEntries call.
type pagedReader interface {
	ReadEntriesPage(ctx context.Context, chatID string, fromSeq int64, limit int) ([]ledger.Entry, error)
}

// readAll pages through store's entries for chatID from fromSeq, in seq
// order, so a big chat is never loaded in one slice when the store supports it.
func readAll(ctx context.Context, store ledger.LedgerStore, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	pr, ok := store.(pagedReader)
	if !ok {
		return store.ReadEntries(ctx, chatID, fromSeq)
	}
	var out []ledger.Entry
	next := fromSeq
	for {
		page, err := pr.ReadEntriesPage(ctx, chatID, next, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
		next = page[len(page)-1].Seq + 1
	}
}

// ArtifactRevision is one materialized (non-aborted) artifact.revision entry.
type ArtifactRevision struct {
	Revision       int
	ParentRevision int
	Kind           string
	Class          string
	Lineage        json.RawMessage
	BytesRef       string
	NodeID, TurnID string
	At             time.Time
	Seq            int64
}

// Artifact is one id's revision chain, oldest first, aborted revisions excluded.
type Artifact struct {
	ID        string
	Revisions []ArtifactRevision
}

// Latest returns the highest-numbered revision, false if none.
func (a *Artifact) Latest() (ArtifactRevision, bool) {
	if a == nil || len(a.Revisions) == 0 {
		return ArtifactRevision{}, false
	}
	return a.Revisions[len(a.Revisions)-1], true
}

// NodeState is keyed by NodeID alone (IDs recur across turns; per-turn keys let a rebuild resurrect stale rows).
// StartedSeq and Terminal* are independent later-wins slots so a rebuild regenerates both start and end rows.
type NodeState struct {
	NodeID, TurnID string
	StartedSeq     int64     // 0 = no node.started entry seen
	StartedAt      time.Time // the source ledger entry's own At, not fold/rebuild wall-clock
	TerminalStatus string    // "" | "done" | "failed" | "cancelled" - "" whenever a start supersedes it
	TerminalSeq    int64
	TerminalAt     time.Time
	Round          int
}

// JudgeRound is one judge_round artifact.revision: the round is the artifact write, folded like any other id.
type JudgeRound struct {
	ID             string
	NodeID, TurnID string
	At             time.Time
	Seq            int64
}

// Result is one chat's ledger folded into its projections.
type Result struct {
	Artifacts   map[string]*Artifact  // by id (Entry.Key)
	Nodes       map[string]*NodeState // by NodeID alone - see NodeState's doc for why not (node_id, turn_id)
	JudgeRounds []JudgeRound          // seq order
	LastSeq     int64

	// MemoryRecalls/MemoryVotes are keyed by memory id. Nothing retracts a recall or vote, so they accumulate
	// directly into Result rather than through the live/finalize rebuild.
	MemoryRecalls map[string]*MemoryRecallState
	MemoryVotes   map[string]*MemoryVoteState
}

// MemoryRecallState is one memory's recall projection within a chat.
type MemoryRecallState struct {
	ID             string
	Recalls        int
	LastRecalledAt time.Time
}

// MemoryVoteState is one memory's vote projection within a chat.
type MemoryVoteState struct {
	ID            string
	Upvotes       int
	Downvotes     int
	LastUpvotedAt time.Time
	// humanVote is the last actor=human direction ("up"/"down"/""): a human vote is a toggle, so re-applying
	// it must undo the prior contribution first. Judge entries never touch it.
	humanVote string
}

// applyHumanVote folds an actor=human memory.vote latest-wins, undoing s.humanVote's prior contribution
// first. supported/contradicted/not_relevant map to up/down/none, the inverse of rest's humanLedgerVote.
func applyHumanVote(s *MemoryVoteState, vote ledger.MemoryVote, at time.Time) {
	switch s.humanVote {
	case "up":
		s.Upvotes--
	case "down":
		s.Downvotes--
	}
	switch vote {
	case ledger.MemoryVoteSupported:
		s.humanVote = "up"
		s.Upvotes++
		if at.After(s.LastUpvotedAt) {
			s.LastUpvotedAt = at
		}
	case ledger.MemoryVoteContradicted:
		s.humanVote = "down"
		s.Downvotes++
	default: // not_relevant: the "none" retraction - no new contribution
		s.humanVote = ""
	}
	if s.Upvotes < 0 {
		s.Upvotes = 0
	}
	if s.Downvotes < 0 {
		s.Downvotes = 0
	}
}

// FoldAbsorption redirects votes/recalls for absorbed ids onto their (chain-resolved) survivor, summing counts
// and keeping the later timestamp. Call after Fold/Apply, before comparing a rebuild against the live store.
func (r *Result) FoldAbsorption(absorbedBy map[string]string) {
	if r == nil || len(absorbedBy) == 0 {
		return
	}
	votes := map[string]*MemoryVoteState{}
	for id, v := range r.MemoryVotes {
		sid := resolveSurvivor(id, absorbedBy)
		s, ok := votes[sid]
		if !ok {
			s = &MemoryVoteState{ID: sid}
			votes[sid] = s
		}
		s.Upvotes += v.Upvotes
		s.Downvotes += v.Downvotes
		if v.LastUpvotedAt.After(s.LastUpvotedAt) {
			s.LastUpvotedAt = v.LastUpvotedAt
		}
	}
	r.MemoryVotes = votes

	recalls := map[string]*MemoryRecallState{}
	for id, v := range r.MemoryRecalls {
		sid := resolveSurvivor(id, absorbedBy)
		s, ok := recalls[sid]
		if !ok {
			s = &MemoryRecallState{ID: sid}
			recalls[sid] = s
		}
		s.Recalls += v.Recalls
		if v.LastRecalledAt.After(s.LastRecalledAt) {
			s.LastRecalledAt = v.LastRecalledAt
		}
	}
	r.MemoryRecalls = recalls
}

// resolveSurvivor follows absorbedBy to its end, guarding against a cyclic map.
func resolveSurvivor(id string, absorbedBy map[string]string) string {
	seen := map[string]bool{}
	for {
		next, ok := absorbedBy[id]
		if !ok || seen[id] {
			return id
		}
		seen[id] = true
		id = next
	}
}

// RecalledIDs returns every memory id this chat's ledger recorded a memory.recall for.
func (r *Result) RecalledIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.MemoryRecalls))
	for id := range r.MemoryRecalls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type revisionPayload struct {
	Revision       int             `json:"revision"`
	ParentRevision int             `json:"parent_revision"`
	Kind           string          `json:"kind"`
	Class          string          `json:"class"`
	Lineage        json.RawMessage `json:"lineage"`
	BytesRef       string          `json:"bytes_ref"`
}

type abortedPayload struct {
	Revision int `json:"revision"`
}

type nodePayload struct {
	NodeID string `json:"node_id"`
	Turn   string `json:"turn"`
	Round  int    `json:"round"`
}

type revKey struct {
	id  string
	rev int
}

// applyLoop requires entries in seq order: a later entry for the same key overrides an earlier one and an
// aborted entry deletes its revision. res and live are mutated in place so Apply can seed them.
func applyLoop(res *Result, live map[revKey]ArtifactRevision, entries []ledger.Entry) {
	for _, e := range entries {
		if e.Seq > res.LastSeq {
			res.LastSeq = e.Seq
		}
		switch e.Kind {
		case ledger.KindArtifactRevision, ledger.KindArtifactRevisionAborted:
			applyArtifactEntry(live, e)
		case ledger.KindNodeStarted, ledger.KindNodeDone, ledger.KindNodeFailed, ledger.KindNodeCancelled:
			applyNodeEntry(res, e)
		case ledger.KindMemoryRecall:
			applyMemoryRecall(res, e)
		case ledger.KindMemoryVote:
			applyMemoryVote(res, e)
		}
	}
}

// applyArtifactEntry: materialize one artifact-revision entry (or its abort) into live.
func applyArtifactEntry(live map[revKey]ArtifactRevision, e ledger.Entry) {
	switch e.Kind {
	case ledger.KindArtifactRevision:
		var p revisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return
		}
		live[revKey{id: e.Key, rev: p.Revision}] = ArtifactRevision{
			Revision: p.Revision, ParentRevision: p.ParentRevision, Kind: p.Kind, Class: p.Class,
			Lineage: p.Lineage, BytesRef: p.BytesRef, NodeID: e.NodeID, TurnID: e.TurnID, At: e.At, Seq: e.Seq,
		}
	default: // KindArtifactRevisionAborted
		var p abortedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return
		}
		delete(live, revKey{id: e.Key, rev: p.Revision})
	}
}

// applyNodeEntry: fold one node lifecycle entry into res.Nodes (keyed by NodeID alone -
// node IDs recur across turns, so turn-keying would resurrect stale state).
func applyNodeEntry(res *Result, e ledger.Entry) {
	var p nodePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return
	}
	key := p.NodeID
	n, ok := res.Nodes[key]
	if !ok {
		n = &NodeState{NodeID: p.NodeID, TurnID: p.Turn}
		res.Nodes[key] = n
	}
	n.Round = p.Round
	n.TurnID = p.Turn
	switch e.Kind {
	case ledger.KindNodeStarted:
		// A later start re-runs the node; entries arrive in seq order, so any
		// earlier terminal necessarily precedes it and is superseded.
		n.TerminalStatus, n.TerminalSeq, n.TerminalAt = "", 0, time.Time{}
		n.StartedSeq, n.StartedAt = e.Seq, e.At
	case ledger.KindNodeDone:
		n.TerminalStatus, n.TerminalSeq, n.TerminalAt = "done", e.Seq, e.At
	case ledger.KindNodeFailed:
		n.TerminalStatus, n.TerminalSeq, n.TerminalAt = "failed", e.Seq, e.At
	case ledger.KindNodeCancelled:
		n.TerminalStatus, n.TerminalSeq, n.TerminalAt = "cancelled", e.Seq, e.At
	}
}

// applyMemoryRecall: fold one recall entry into res.MemoryRecalls.
func applyMemoryRecall(res *Result, e ledger.Entry) {
	var p ledger.MemoryRecallPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return
	}
	for _, m := range p.Entries {
		s, ok := res.MemoryRecalls[m.ID]
		if !ok {
			s = &MemoryRecallState{ID: m.ID}
			res.MemoryRecalls[m.ID] = s
		}
		s.Recalls++
		if e.At.After(s.LastRecalledAt) {
			s.LastRecalledAt = e.At
		}
	}
}

// applyMemoryVote: fold one vote entry into res.MemoryVotes.
func applyMemoryVote(res *Result, e ledger.Entry) {
	var p ledger.MemoryVotePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return
	}
	s, ok := res.MemoryVotes[p.MemoryID]
	if !ok {
		s = &MemoryVoteState{ID: p.MemoryID}
		res.MemoryVotes[p.MemoryID] = s
	}
	if p.Actor == "human" {
		applyHumanVote(s, p.Vote, e.At)
		return
	}
	switch p.Vote {
	case ledger.MemoryVoteSupported:
		s.Upvotes++
		if e.At.After(s.LastUpvotedAt) {
			s.LastUpvotedAt = e.At
		}
	case ledger.MemoryVoteContradicted:
		s.Downvotes++
	}
}

// finalize rebuilds res.Artifacts and res.JudgeRounds from live, always from scratch, since an aborted entry
// can remove a revision a prior Result already held.
func finalize(res *Result, live map[revKey]ArtifactRevision) *Result {
	res.Artifacts = map[string]*Artifact{}
	res.JudgeRounds = nil
	for k, rv := range live {
		a, ok := res.Artifacts[k.id]
		if !ok {
			a = &Artifact{ID: k.id}
			res.Artifacts[k.id] = a
		}
		a.Revisions = append(a.Revisions, rv)
	}
	for id, a := range res.Artifacts {
		sort.Slice(a.Revisions, func(i, j int) bool { return a.Revisions[i].Revision < a.Revisions[j].Revision })
		for _, r := range a.Revisions {
			// The judge round is the artifact write itself, so JudgeRounds is a view over it.
			if r.Kind == judgeRoundKind {
				res.JudgeRounds = append(res.JudgeRounds, JudgeRound{ID: id, NodeID: r.NodeID, TurnID: r.TurnID, At: r.At, Seq: r.Seq})
			}
		}
	}
	sort.Slice(res.JudgeRounds, func(i, j int) bool { return res.JudgeRounds[i].Seq < res.JudgeRounds[j].Seq })
	return res
}

// ApplyEntries folds entries already in hand from scratch. After a kind-filtered read, LastSeq covers
// only those kinds, so never persist it as a watermark (a later fold would skip the rest).
func ApplyEntries(entries []ledger.Entry) *Result {
	res := newResult()
	live := map[revKey]ArtifactRevision{}
	applyLoop(res, live, entries)
	return finalize(res, live)
}

// RequiredKinds are the only kinds applyLoop reads, so a caller fetching entries itself can skip the much
// larger agent.invoke/llm.call/otel payloads.
var RequiredKinds = []string{
	ledger.KindArtifactRevision, ledger.KindArtifactRevisionAborted,
	ledger.KindNodeStarted, ledger.KindNodeDone, ledger.KindNodeFailed, ledger.KindNodeCancelled,
	ledger.KindMemoryRecall, ledger.KindMemoryVote,
}

// newResult allocates every Result map so applyLoop can write into them
// unconditionally regardless of which entry kinds a chat actually has.
func newResult() *Result {
	return &Result{
		Artifacts:     map[string]*Artifact{},
		Nodes:         map[string]*NodeState{},
		MemoryRecalls: map[string]*MemoryRecallState{},
		MemoryVotes:   map[string]*MemoryVoteState{},
	}
}

// Fold folds every entry for chatID from fromSeq; a convenience over Apply.
func Fold(ctx context.Context, store ledger.LedgerStore, chatID string, fromSeq int64) (*Result, error) {
	return Apply(ctx, store, chatID, fromSeq-1)
}

// Apply folds only entries newer than from (a projection's watermark); from=-1 folds the whole chat.
func Apply(ctx context.Context, store ledger.LedgerStore, chatID string, from int64) (*Result, error) {
	return ApplySeeded(ctx, store, chatID, nil, from)
}

// ApplySeeded is Apply starting from a checkpoint Result. Seeding applies only when seed.LastSeq > from, so
// pass the caller's own watermark (0 or -1), never seed.LastSeq. A stale checkpoint just re-folds a few entries.
func ApplySeeded(ctx context.Context, store ledger.LedgerStore, chatID string, seed *Result, from int64) (*Result, error) {
	live := map[revKey]ArtifactRevision{}
	nodes := map[string]*NodeState{}
	res := newResult()
	// A stale seed the caller's from already supersedes must contribute nothing, memory counts included.
	if seed != nil && seed.LastSeq > from {
		seedFold(seed, live, nodes)
		res.MemoryRecalls, res.MemoryVotes = seedMemory(seed)
		from = seed.LastSeq
	}
	entries, err := readAll(ctx, store, chatID, from+1)
	if err != nil {
		return nil, err
	}
	res.Nodes = nodes
	applyLoop(res, live, entries)
	return finalize(res, live), nil
}

// seedMemory deep-copies a prior Result's memory projections so ApplySeeded
// can keep accumulating into them instead of dropping counts already folded.
func seedMemory(seed *Result) (map[string]*MemoryRecallState, map[string]*MemoryVoteState) {
	recalls := map[string]*MemoryRecallState{}
	for id, s := range seed.MemoryRecalls {
		cp := *s
		recalls[id] = &cp
	}
	votes := map[string]*MemoryVoteState{}
	for id, s := range seed.MemoryVotes {
		cp := *s
		votes[id] = &cp
	}
	return recalls, votes
}

// seedFold populates live/nodes from a previously folded Result, so
// ApplySeeded can resume from a checkpoint instead of an empty state.
func seedFold(seed *Result, live map[revKey]ArtifactRevision, nodes map[string]*NodeState) {
	for id, a := range seed.Artifacts {
		for _, rv := range a.Revisions {
			live[revKey{id: id, rev: rv.Revision}] = rv
		}
	}
	for id, n := range seed.Nodes {
		cp := *n
		nodes[id] = &cp
	}
}
