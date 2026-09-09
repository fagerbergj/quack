// Judge votes on recalled memories: the worker's received set is rendered
// for the judge (mirrors findings.go's per-finding verification), and a
// gate-passed round's votes are applied to the memory store (epic P1).
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

// planJudgeMemoryHeader: unlike judgeMemoriesInstructions, these are NOT
// voted on - the plan judge scores plan shape, not delivered work, so there
// is nothing yet to check a memory against (epic P2).
const planJudgeMemoryHeader = "PROJECT MEMORY - background notes about this repo/task family, for context only. Do not vote on these; there is no submit_plan_verdict field for them.\n\n"

// planMemorySection renders top-k memories for the plan judge's prompt -
// the receivedMemoriesSection twin for a round that never votes.
func planMemorySection(hits []memory.Delivered) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(planJudgeMemoryHeader)
	for _, m := range hits {
		fmt.Fprintf(&sb, "- id=%s: %s\n", m.ID, m.Content)
	}
	sb.WriteString("\n")
	return sb.String()
}

// memoryIDs extracts ids from a received set, for the round-scoped tool
// description, force-close instruction, and nudge text .
func memoryIDs(received []memory.Delivered) []string {
	ids := make([]string, len(received))
	for i, m := range received {
		ids[i] = m.ID
	}
	return ids
}

// missingMemoryVotes reports a round that owed votes (non-empty received
// set) but whose verdict left at least one of them unvoted . A
// partial vote (e.g. 1 of 5) used to read as "not missing" since only
// len(v.Memories)==0 was checked - the rest silently never got a vote
// recorded (applyMemoryVotesOnPass only applies ids present in v.Memories).
func missingMemoryVotes(receivedIDs []string, v verdict) bool {
	if len(receivedIDs) == 0 {
		return false
	}
	voted := make(map[string]bool, len(v.Memories))
	for _, mv := range v.Memories {
		voted[mv.ID] = true
	}
	for _, id := range receivedIDs {
		if !voted[id] {
			return true
		}
	}
	return false
}

// judgeMemoriesNudgeText: one-shot in-session nudge for a
// verdict that reached submit_verdict/text-JSON but voted on nothing.
func judgeMemoriesNudgeText(receivedIDs []string) string {
	return fmt.Sprintf("You did not vote on the recalled memories. Vote on memories %s via submit_verdict's `memories` array before finishing.", strings.Join(receivedIDs, ", "))
}

// mergeMemoryHits appends new into base, deduping by id (first occurrence
// wins) so a memory recalled by both prefill and a recall_memory tool call
// is voted on once, not twice (epic P2 adversarial review finding).
func mergeMemoryHits(base, add []memory.Delivered) []memory.Delivered {
	if len(add) == 0 {
		return base
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
	}
	return base
}

// recallLedgerEntry appends a best-effort memory.recall ledger entry for one
// injection (design decision P1: the ledger is the source of truth for
// what a chat retrieved). Best-effort, unlike memory.vote below: it records
// a delivery that already happened, and nothing is projected from it in the
// hot path (recalls/last_recalled_at are bumped directly by the caller,
// independent of this entry) - same observational pattern as appendNodeEvent.
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
	// Agent/Round are stamped explicitly from cfg/the round argument, not read
	// off ctx - a ctx value set inside a node body never crosses the RunNode
	// scheduling boundary (same SetLedgerCoords discipline as the worker/judge
	// model stamps; #1259).
	if _, err := cfg.Ledger.AppendIntent(ctx, ledger.Entry{
		ChatID: cfg.ChatID, NodeID: nodeID, Agent: cfg.Agent, Round: strconv.Itoa(round),
		Kind: ledger.KindMemoryRecall, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("ledger memory.recall append failed (observational; run unaffected)", "component", "vetting", "node", nodeID, "err", err)
	}
}

// applyMemoryVotesOnPass applies the round's votes to the memory store -
// called only after the gate passes (failed rounds record nothing, per
// P1). received scopes which ids are even eligible: a vote for an id
// the worker was never given is dropped rather than trusted blindly.
//
// memory.vote is fail-closed, same discipline as artifact.revision: the
// point mutation (upvotes/score/tier) is a PROJECTION of the ledger entry,
// so the entry is appended via AppendIntent BEFORE the point is touched. A
// failed append skips that vote's mutation entirely (logged, not applied) -
// unlike memory.recall (recallLedgerEntry), nothing else backs this write.
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
