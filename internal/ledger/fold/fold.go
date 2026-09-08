// Package fold projects a chat's ledger.Entry stream into read models (V4
// §4.9): the artifact revision chain per id, node lifecycle state, and judge
// rounds. This is the ONE definition of the aborted/later-entry-wins rule -
// recordstore.lastRevision used to duplicate it inline before #1101.
package fold

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// pageSize bounds one page when the store supports paging (PGStore) -
// ponytail: fixed size, no adaptive backoff. var, not const, so tests can
// shrink it to exercise the multi-page path without 1000+ fixture rows.
var pageSize = 1000

// judgeRoundKind is vetting's kindJudgeRound artifact kind name, duplicated
// here (not imported) to keep fold dependency-free of vetting.
const judgeRoundKind = "judge_round"

// pagedReader is implemented by a LedgerStore that can page results
// server-side (PGStore.ReadEntriesPage); a store without it is
// read in one ReadEntries call.
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

// NodeState is one NODE's (not node+turn's) lifecycle, keyed by NodeID
// alone - a turn is fresh per invocation (uuid.NewString() at
// server/rest/handler.go, serve/extensions.go) while the WAL is per-chat
// lifetime and node IDs are only unique within one plan, so the same node
// ID legitimately recurs across turns; keying by turn would keep a stale
// turn's state around forever, which rebuild (matching the live, per-run
// table by node id + event name alone) could resurrect as a spurious row.
// TurnID reflects the MOST RECENT entry only. Richer per-round fields
// (tokens, output, model) never made it into the skinny node.* payload, so
// this is a lossy reconstruction, not a byte-for-byte replay of the SSE
// table's real events. StartedSeq and Terminal* are tracked INDEPENDENTLY
// (each is its own "later entry wins" slot, same rule as an artifact
// revision key) so a node that has already reached done/failed still
// reports its node.started seq too - #1121: a rebuild must be able to
// regenerate BOTH the node_start and node_done/failed rows, not just the
// terminal one. A later node.started clears any earlier terminal (entries
// arrive in seq order, so the terminal necessarily precedes a re-run's
// start) - otherwise a completed run's node would still carry a PRIOR
// turn's stale terminal status.
type NodeState struct {
	NodeID, TurnID string
	StartedSeq     int64  // 0 = no node.started entry seen
	TerminalStatus string // "" | "done" | "failed" - "" whenever a start supersedes it
	TerminalSeq    int64
	Round          int
}

// JudgeRound is one judge_round artifact.revision (#1144 P2: no dedicated
// entry kind - the round IS the artifact write, folded like any other id).
type JudgeRound struct {
	ID             string
	NodeID, TurnID string
	At             time.Time
	Seq            int64
}

// Result is one chat's ledger folded into its projections (V4 §4.9).
type Result struct {
	Artifacts   map[string]*Artifact  // by id (Entry.Key)
	Nodes       map[string]*NodeState // by NodeID alone - see NodeState's doc for why not (node_id, turn_id)
	JudgeRounds []JudgeRound          // seq order
	LastSeq     int64

	// MemoryRecalls/MemoryVotes (epic #1255 P1): by memory id, folded from
	// memory.recall/memory.vote entries. Pure counters - unlike artifacts,
	// nothing ever retracts a recall or a vote, so they accumulate directly
	// into Result rather than going through the live/finalize rebuild.
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
}

// RecalledIDs returns every memory id this chat's ledger recorded a
// memory.recall for - the recalled set applyMemoryOutcome (design decision
// #1255) reinforces/invalidates instead of the memories minted in the chat.
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

// applyLoop is the fold's one true loop: entries MUST be in seq order (as
// every ReadEntries*/readAll path returns them), since a later entry for the
// same (id, revision) or (node, turn) key overrides an earlier one - an
// artifact.revision.aborted deletes its revision from live, and a retried
// save that reuses the same number (see ledger.KindArtifactRevisionAborted's
// doc) re-adds it. res and live are mutated in place so Apply can seed them
// from a prior Result instead of always starting empty.
func applyLoop(res *Result, live map[revKey]ArtifactRevision, entries []ledger.Entry) {
	for _, e := range entries {
		if e.Seq > res.LastSeq {
			res.LastSeq = e.Seq
		}
		switch e.Kind {
		case ledger.KindArtifactRevision:
			var p revisionPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			live[revKey{id: e.Key, rev: p.Revision}] = ArtifactRevision{
				Revision: p.Revision, ParentRevision: p.ParentRevision, Kind: p.Kind, Class: p.Class,
				Lineage: p.Lineage, BytesRef: p.BytesRef, NodeID: e.NodeID, TurnID: e.TurnID, At: e.At, Seq: e.Seq,
			}
		case ledger.KindArtifactRevisionAborted:
			var p abortedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			delete(live, revKey{id: e.Key, rev: p.Revision})
		case ledger.KindNodeStarted, ledger.KindNodeDone, ledger.KindNodeFailed:
			var p nodePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			// Keyed by NodeID ALONE, not (NodeID, Turn): a turn is fresh
			// per invocation (uuid.NewString() at server/rest/handler.go,
			// serve/extensions.go), the WAL is per-chat lifetime, and node
			// IDs are only unique within one plan - so the SAME node ID
			// legitimately recurs across turns. Keying by turn kept a
			// stale state around forever (turn 1's node_failed surviving
			// next to turn 2's node_done for the same node), which
			// rebuild - matching the live, per-run table by (node id,
			// event name) alone - could resurrect as a spurious row.
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
				// A later start re-runs the node; entries arrive in seq
				// order, so any earlier terminal (same-turn retry or a
				// previous turn) necessarily precedes it and is superseded.
				n.TerminalStatus, n.TerminalSeq = "", 0
				n.StartedSeq = e.Seq
			case ledger.KindNodeDone:
				n.TerminalStatus, n.TerminalSeq = "done", e.Seq
			case ledger.KindNodeFailed:
				n.TerminalStatus, n.TerminalSeq = "failed", e.Seq
			}
		case ledger.KindMemoryRecall:
			var p ledger.MemoryRecallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
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
		case ledger.KindMemoryVote:
			var p ledger.MemoryVotePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			s, ok := res.MemoryVotes[p.MemoryID]
			if !ok {
				s = &MemoryVoteState{ID: p.MemoryID}
				res.MemoryVotes[p.MemoryID] = s
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
	}
}

// finalize rebuilds res.Artifacts and res.JudgeRounds from live - the
// materialized (non-aborted) revision set - after applyLoop has run. Always
// rebuilt from scratch (never incrementally patched) because an
// artifact.revision.aborted entry can remove a revision that a prior
// Result's Artifacts map already held.
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
			// judge_round has no dedicated entry kind (#1144 P2): the round
			// IS the artifact write, so JudgeRounds is a view over it, not a
			// second fold input.
			if r.Kind == judgeRoundKind {
				res.JudgeRounds = append(res.JudgeRounds, JudgeRound{ID: id, NodeID: r.NodeID, TurnID: r.TurnID, At: r.At, Seq: r.Seq})
			}
		}
	}
	sort.Slice(res.JudgeRounds, func(i, j int) bool { return res.JudgeRounds[i].Seq < res.JudgeRounds[j].Seq })
	return res
}

// applyEntries folds entries from scratch - Fold's and LastRevision's shared path.
func applyEntries(entries []ledger.Entry) *Result {
	res := newResult()
	live := map[revKey]ArtifactRevision{}
	applyLoop(res, live, entries)
	return finalize(res, live)
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

// Fold reads every entry for chatID from fromSeq (in seq-order pages) and
// folds them into Result. A fromSeq=0 convenience over Apply, the one fold
// entry point (#1144 P3) - every catch-up (SSE, artifact, node_state) reads
// from a watermark through Apply instead of its own loop.
func Fold(ctx context.Context, store ledger.LedgerStore, chatID string, fromSeq int64) (*Result, error) {
	return Apply(ctx, store, chatID, fromSeq-1)
}

// Apply folds only the entries newer than from (a projection's watermark),
// instead of re-folding chatID's whole history (#1144 P3). from=-1 (via
// Fold's fromSeq=0) is a fresh fold of the whole chat.
func Apply(ctx context.Context, store ledger.LedgerStore, chatID string, from int64) (*Result, error) {
	return ApplySeeded(ctx, store, chatID, nil, from)
}

// ApplySeeded is Apply, but starting from a previously folded Result (a
// checkpoint) instead of empty state (#1144 P5). seed nil is exactly Apply.
// from is the caller's OWN already-known watermark, not the seed's - pass 0
// (or -1, Fold's convention) when the seed is the only state you have, e.g.
// internal/store.Store.WriteCheckpoint. Seeding is keyed off seed's OWN
// LastSeq compared against that from, so it only activates when the seed is
// actually newer: passing seed.LastSeq as `from` itself would make the
// `seed.LastSeq > from` check false and silently skip seeding. A caller
// that reads a checkpoint row written by a concurrent, slightly-behind turn
// still folds correctly, just re-processes a few more entries than the
// freshest checkpoint would have needed (see internal/store.Checkpoint's
// doc for where that row now lives - #1144 P5 review moved it out of the ledger).
func ApplySeeded(ctx context.Context, store ledger.LedgerStore, chatID string, seed *Result, from int64) (*Result, error) {
	live := map[revKey]ArtifactRevision{}
	nodes := map[string]*NodeState{}
	res := newResult()
	// Same "seed.LastSeq > from" gate as live/nodes above (not just seed !=
	// nil, #1257 review): a STALE seed - one the caller's own from already
	// supersedes - must contribute nothing, memory projections included, or
	// a stale checkpoint's counts get seeded back in even though the caller
	// already knows better.
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

// LastRevision returns id's highest materialized revision in chatID's
// ledger, 0 if none - a diagnostic/recovery helper (recordstore's own save
// path no longer calls this; #1144 P4 reads the artifact store's real
// latest instead - see recordstore.saveAt's doc).
func LastRevision(ctx context.Context, store ledger.LedgerStore, chatID, id string) (int, error) {
	entries, err := readAll(ctx, store, chatID, 0)
	if err != nil {
		return 0, err
	}
	res := applyEntries(entries)
	rev, _ := res.Artifacts[id].Latest()
	return rev.Revision, nil
}
