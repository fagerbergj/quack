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

// pagedReader is implemented by a LedgerStore that can page results
// server-side (PGStore.ReadEntriesPage); a store without it (FSStore) is
// read in one ReadEntries call - it already holds its whole JSONL in memory.
type pagedReader interface {
	ReadEntriesPage(ctx context.Context, chatID string, fromSeq int64, limit int) ([]ledger.Entry, error)
}

// keyedReader is implemented by a LedgerStore that can filter by key
// server-side (PGStore.ReadEntriesByKey, backed by the (chat_id, key)
// index); a store without it is scanned in full and filtered in memory.
type keyedReader interface {
	ReadEntriesByKey(ctx context.Context, chatID, key string, fromSeq int64) ([]ledger.Entry, error)
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

// NodeState is one node+turn's lifecycle, derived from the last node.* entry
// seen for it - richer per-round fields (tokens, output, model) never made
// it into the skinny node.* payload, so this is a lossy reconstruction, not
// a byte-for-byte replay of the SSE table's node_done event.
type NodeState struct {
	NodeID, TurnID string
	Status         string // "started", "done", "failed"
	Round          int
	At             time.Time
	Seq            int64
}

// JudgeRound is one judge.round entry.
type JudgeRound struct {
	ID             string
	NodeID, TurnID string
	Payload        json.RawMessage
	At             time.Time
	Seq            int64
}

// Result is one chat's ledger folded into its projections (V4 §4.9).
type Result struct {
	Artifacts   map[string]*Artifact  // by id (Entry.Key)
	Nodes       map[string]*NodeState // by node_id+"\x00"+turn_id
	JudgeRounds []JudgeRound          // seq order
	LastSeq     int64
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

// applyEntries is the fold's one true loop: entries MUST be in seq order (as
// every ReadEntries*/readAll path returns them), since a later entry for the
// same (id, revision) or (node, turn) key overrides an earlier one - an
// artifact.revision.aborted deletes its revision from live, and a retried
// save that reuses the same number (see ledger.KindArtifactRevisionAborted's
// doc) re-adds it.
func applyEntries(entries []ledger.Entry) *Result {
	res := &Result{Artifacts: map[string]*Artifact{}, Nodes: map[string]*NodeState{}}
	live := map[revKey]ArtifactRevision{}

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
			status := "started"
			if e.Kind == ledger.KindNodeDone {
				status = "done"
			} else if e.Kind == ledger.KindNodeFailed {
				status = "failed"
			}
			res.Nodes[p.NodeID+"\x00"+p.Turn] = &NodeState{NodeID: p.NodeID, TurnID: p.Turn, Status: status, Round: p.Round, At: e.At, Seq: e.Seq}
		case ledger.KindJudgeRound:
			res.JudgeRounds = append(res.JudgeRounds, JudgeRound{ID: e.Key, NodeID: e.NodeID, TurnID: e.TurnID, Payload: e.Payload, At: e.At, Seq: e.Seq})
		}
	}

	for k, rv := range live {
		a, ok := res.Artifacts[k.id]
		if !ok {
			a = &Artifact{ID: k.id}
			res.Artifacts[k.id] = a
		}
		a.Revisions = append(a.Revisions, rv)
	}
	for _, a := range res.Artifacts {
		sort.Slice(a.Revisions, func(i, j int) bool { return a.Revisions[i].Revision < a.Revisions[j].Revision })
	}
	sort.Slice(res.JudgeRounds, func(i, j int) bool { return res.JudgeRounds[i].Seq < res.JudgeRounds[j].Seq })
	return res
}

// Fold reads every entry for chatID from fromSeq (in seq-order pages) and
// folds them into Result.
func Fold(ctx context.Context, store ledger.LedgerStore, chatID string, fromSeq int64) (*Result, error) {
	entries, err := readAll(ctx, store, chatID, fromSeq)
	if err != nil {
		return nil, err
	}
	return applyEntries(entries), nil
}

// LastRevision returns id's highest materialized revision in chatID's
// ledger, 0 if none - recordstore.save's ParentRevision source. Uses the
// (chat_id, key) index via ReadEntriesByKey when the store supports it
// (PGStore), instead of folding the whole chat.
func LastRevision(ctx context.Context, store ledger.LedgerStore, chatID, id string) (int, error) {
	var entries []ledger.Entry
	var err error
	if kr, ok := store.(keyedReader); ok {
		entries, err = kr.ReadEntriesByKey(ctx, chatID, id, 0)
	} else {
		entries, err = readAll(ctx, store, chatID, 0)
	}
	if err != nil {
		return 0, err
	}
	res := applyEntries(entries)
	rev, _ := res.Artifacts[id].Latest()
	return rev.Revision, nil
}
