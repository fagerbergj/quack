package vetting

import (
	"testing"

	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// TestJudgeSystemPromptRoundInvariant proves submit_verdict's description (and
// so the judge's whole system prompt, built once per round by promptbuilder)
// stays byte-identical whether or not this round's recall_memory hits added a
// new id - the ids belong in the per-round user prompt's trailing
// receivedMemoriesSection, not the system prompt, so a fresh memory id never
// evicts the system-prompt prefix cache mid-node.
func TestJudgeSystemPromptRoundInvariant(t *testing.T) {
	var sink verdict
	build := func(ids []string) string {
		st, err := newSubmitVerdictTool(&sink, ids)
		if err != nil {
			t.Fatal(err)
		}
		return promptbuilder.Judge([]tool.Tool{st}, judgeBehaviour(true, true))
	}
	round1 := build([]string{"m1", "m2"})
	round2 := build([]string{"m1", "m2", "m3"}) // round 2's revise called recall_memory and added m3
	noMemories := build(nil)

	if round1 != round2 {
		t.Fatalf("judge system prompt varies with recalled-memory ids:\nround1=%q\nround2=%q", round1, round2)
	}
	if round1 != noMemories {
		t.Fatalf("judge system prompt varies with presence/absence of received memories:\nwith=%q\nwithout=%q", round1, noMemories)
	}
}
