// Judge votes on recalled memories: the worker's received set is rendered
// for the judge (mirrors findings.go's per-finding verification), and a
// gate-passed round's votes are applied to the memory store (epic #1255 P1).
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

// memoryVerdict: one recalled memory's vote. Mirrors findingVerdict's shape.
type memoryVerdict struct {
	ID     string `json:"id" jsonschema:"the recalled memory's id, copied verbatim from the RECALLED MEMORIES block"`
	Vote   string `json:"vote" jsonschema:"supported (the delivered work backs this memory), contradicted (the work shows the opposite), or not_relevant (this memory had nothing to do with the task)"`
	Reason string `json:"reason,omitempty" jsonschema:"one sentence citing what in the answer supports the vote"`
}

// judgeMemoriesInstructions: tells the judge to vote on every delivered memory.
const judgeMemoriesInstructions = "RECALLED MEMORIES - the worker was given these before it started; vote on EVERY one in submit_verdict's `memories` array as {id, vote, reason}: `supported` if the delivered work is consistent with (or confirms) the memory, `contradicted` if the work shows the memory is wrong, `not_relevant` if the memory had nothing to do with this task. Votes are only recorded when the round passes the gate.\n\n"

// receivedMemoriesSection renders the worker's recalled set for the judge -
// the memories.go/findings.go twin, but sourced from what recall actually
// delivered rather than staged findings.
func receivedMemoriesSection(received []memory.Delivered) string {
	if len(received) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(judgeMemoriesInstructions)
	for _, m := range received {
		fmt.Fprintf(&sb, "- id=%s: %s\n", m.ID, m.Content)
	}
	sb.WriteString("\n")
	return sb.String()
}

// recallLedgerEntry appends a best-effort memory.recall ledger entry for one
// injection (design decision #1255 P1: the ledger is the source of truth for
// what a chat retrieved). Never fails the node - same observational pattern
// as appendNodeEvent.
func recallLedgerEntry(ctx context.Context, cfg Config, nodeID string, round int, source string, received []memory.Delivered) {
	if cfg.Ledger == nil || len(received) == 0 {
		return
	}
	entries := make([]ledger.MemoryRecallEntry, len(received))
	for i, m := range received {
		entries[i] = ledger.MemoryRecallEntry{ID: m.ID, Score: m.Score}
	}
	payload, err := json.Marshal(ledger.MemoryRecallPayload{Source: source, Round: round, Entries: entries})
	if err != nil {
		return
	}
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, NodeID: nodeID, Kind: ledger.KindMemoryRecall, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger memory.recall append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "err", err)
	}
}

// applyMemoryVotesOnPass logs a memory.vote ledger entry per vote and
// applies the round's votes to the memory store - called only after the
// gate passes (failed rounds record nothing, per #1255 P1). received scopes
// which ids are even eligible: a vote for an id the worker was never given
// is dropped rather than trusted blindly.
func applyMemoryVotesOnPass(ctx context.Context, cfg Config, nodeID string, round int, received []memory.Delivered, votes []memoryVerdict) {
	if cfg.Memory == nil || len(votes) == 0 || len(received) == 0 {
		return
	}
	knownIDs := make(map[string]bool, len(received))
	for _, m := range received {
		knownIDs[m.ID] = true
	}
	applied := make([]memory.Vote, 0, len(votes))
	for _, v := range votes {
		id := strings.TrimSpace(v.ID)
		if id == "" || !knownIDs[id] {
			continue
		}
		vote := strings.ToLower(strings.TrimSpace(v.Vote))
		switch vote {
		case memory.VoteSupported, memory.VoteContradicted, memory.VoteNotRelevant:
		default:
			continue
		}
		applied = append(applied, memory.Vote{MemoryID: id, Vote: vote, Reason: v.Reason, Actor: memory.ActorJudge})
		if cfg.Ledger != nil {
			payload, err := json.Marshal(ledger.MemoryVotePayload{MemoryID: id, Vote: ledger.MemoryVote(vote), Reason: v.Reason, Actor: string(memory.ActorJudge), Round: round})
			if err == nil {
				if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
					ChatID: cfg.ChatID, NodeID: nodeID, Kind: ledger.KindMemoryVote, At: time.Now().UTC(), Payload: payload,
				}); err != nil {
					slog.Warn("ledger memory.vote append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "err", err)
				}
			}
		}
	}
	if len(applied) == 0 {
		return
	}
	if _, err := cfg.Memory.ApplyVotes(ctx, applied, memory.DefaultInvalidateThreshold); err != nil {
		slog.Warn("apply memory votes failed", "component", "vetting", "node", nodeID, "err", err)
	}
}
