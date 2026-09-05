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
// server-side (PGStore.ReadEntriesPage); a store without it is
// read in one ReadEntries call.
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
	Nodes       map[string]*NodeState // by NodeID alone - see NodeState's doc for why not (node_id, turn_id)
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
