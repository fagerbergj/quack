// Judge votes on recalled memories: the worker's received set is shown to the judge, and a
// gate-passed round's votes are applied to the memory store.
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
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

// memorySection renders header plus one "- id=...: content" line per entry.
func memorySection(header string, items []memory.Delivered) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(header)
	for _, m := range items {
		fmt.Fprintf(&sb, "- id=%s: %s\n", m.ID, m.Content)
	}
	sb.WriteString("\n")
	return sb.String()
}

// receivedMemoriesSection renders what recall actually delivered to the worker, for the judge to vote on.
func receivedMemoriesSection(received []memory.Delivered) string {
	return memorySection(judgeMemoriesInstructions, received)
}

// planJudgeMemoryHeader: these are NOT voted on; the plan judge scores plan shape, so there is no
// delivered work to check a memory against.
const planJudgeMemoryHeader = "PROJECT MEMORY - background notes about this repo/task family, for context only. Do not vote on these; there is no submit_plan_verdict field for them.\n\n"

// planMemorySection renders top-k memories for the plan judge's prompt, without a vote.
func planMemorySection(hits []memory.Delivered) string {
	return memorySection(planJudgeMemoryHeader, hits)
}

// memoryIDs: ids for the round-scoped tool description, force-close instruction and nudge text.
func memoryIDs(received []memory.Delivered) []string {
	ids := make([]string, len(received))
	for i, m := range received {
		ids[i] = m.ID
	}
	return ids
}

// owedMemoryVoteIDs returns the received ids v left unvoted, in receivedIDs order, so a partial vote
// is never told to re-vote what it already recorded.
func owedMemoryVoteIDs(receivedIDs []string, v verdict) []string {
	if len(receivedIDs) == 0 {
		return nil
	}
	voted := make(map[string]bool, len(v.Memories))
	for _, mv := range v.Memories {
		voted[mv.ID] = true
	}
	var owed []string
	for _, id := range receivedIDs {
		if !voted[id] {
			owed = append(owed, id)
		}
	}
	return owed
}

// missingMemoryVotes: the round owed votes but left at least one unvoted. A partial vote counts as
// missing, since applyMemoryVotesOnPass only applies ids present in v.Memories.
func missingMemoryVotes(receivedIDs []string, v verdict) bool {
	return len(owedMemoryVoteIDs(receivedIDs, v)) > 0
}

// judgeMemoriesNudgeText: one-shot in-session nudge naming only the still-owed ids.
func judgeMemoriesNudgeText(owedIDs []string) string {
	return fmt.Sprintf("You did not vote on all the recalled memories. Vote on memories %s via submit_verdict's `memories` array before finishing.", strings.Join(owedIDs, ", "))
}

// mergeMemoryHits appends new into base deduped by id, so a memory is voted on once; added is the
// newly-added subset, the caller's cue to bump recalls once.
func mergeMemoryHits(base, add []memory.Delivered) (merged, added []memory.Delivered) {
	if len(add) == 0 {
		return base, nil
	}
	seen := make(map[string]bool, len(base))
	for _, m := range base {
		seen[m.ID] = true
	}
	for _, m := range add {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		base = append(base, m)
		added = append(added, m)
	}
	return base, added
}

// mergeAndCountRecalledMemories merges add and a live ACP session's Recalled snapshot into received,
// bumping recalls only for new ids, so a judge-less dispatch still counts and nothing double-counts.
func mergeAndCountRecalledMemories(ctx context.Context, cfg Config, advisorToken string, received, add []memory.Delivered) []memory.Delivered {
	var addedIDs []memory.Delivered
	received, addedIDs = mergeMemoryHits(received, add)
	cfg.Memory.RecordRecall(ctx, memoryIDs(addedIDs))
	if advisorToken == "" {
		return received
	}
	t, ok := LookupAdvisorThread(advisorToken)
	if !ok || t.MemSecret == "" {
		return received
	}
	ms, ok := LookupMemSession(t.MemSecret)
	if !ok || ms.Recalled == nil {
		return received
	}
	received, addedIDs = mergeMemoryHits(received, ms.Recalled.Snapshot())
	cfg.Memory.RecordRecall(ctx, memoryIDs(addedIDs))
	return received
}

// recallLedgerEntry appends a best-effort memory.recall entry. Unlike memory.vote it may fail silently:
// it records a delivery that already happened and nothing is projected from it.
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
	// Agent/Round come from cfg and the round argument, not ctx: a ctx value set inside a node body never
	// crosses the RunNode scheduling boundary.
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, NodeID: nodeID, Agent: cfg.Agent, Round: strconv.Itoa(round),
		Kind: ledger.KindMemoryRecall, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger memory.recall append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "err", err)
	}
}

// applyMemoryVotesOnPass applies votes only after the gate passes, and only for ids the worker received.
// memory.vote is fail-closed: the point mutation projects the ledger entry, so a failed append skips it.
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
		if cfg.Ledger != nil {
			payload, err := json.Marshal(ledger.MemoryVotePayload{MemoryID: id, Vote: ledger.MemoryVote(vote), Reason: v.Reason, Actor: string(memory.ActorJudge), Round: round})
			if err != nil {
				slog.Warn("marshal memory.vote payload failed; vote skipped", "component", "vetting", "node", nodeID, "memory_id", id, "err", err)
				continue
			}
			if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
				ChatID: cfg.ChatID, NodeID: nodeID, Kind: ledger.KindMemoryVote, At: time.Now().UTC(), Payload: payload,
			}); err != nil {
				slog.Warn("ledger memory.vote append failed; vote skipped (fail-closed)", "component", "vetting", "node", nodeID, "memory_id", id, "err", err)
				continue
			}
		}
		applied = append(applied, memory.Vote{MemoryID: id, Vote: vote, Reason: v.Reason, Actor: memory.ActorJudge})
	}
	if len(applied) == 0 {
		return
	}
	if _, err := cfg.Memory.ApplyVotes(ctx, applied, memory.DefaultInvalidateThreshold); err != nil {
		slog.Warn("apply memory votes failed", "component", "vetting", "node", nodeID, "err", err)
		return
	}
	slog.Info("memory votes applied", "component", "vetting", "node", nodeID, "round", round, "applied", len(applied))
}
