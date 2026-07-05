package rest

import (
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
)

// TestBuildTurnUsage covers PR2 item 2: buildTurn must populate Turn.usage from
// the orchestrator's own accumulated token counts (store.TurnContent, itself
// summed from stored ADK session events — see store.groupSessionEvents).
// input_tokens = prompt; output_tokens folds candidates + reasoning together
// (schema.Usage has no separate reasoning field).
func TestBuildTurnUsage(t *testing.T) {
	tc := store.TurnContent{
		ID:               "t1",
		CreatedAt:        time.Now(),
		UserText:         "what is the tallest mountain?",
		AsstText:         "Mount Everest.",
		PromptTokens:     40,
		CompletionTokens: 15,
		ReasoningTokens:  2,
		Model:            "gpt-oss-120b",
	}

	turn := buildTurn(tc)

	if turn.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	if turn.Usage.InputTokens == nil || *turn.Usage.InputTokens != 40 {
		t.Errorf("InputTokens = %v, want 40", turn.Usage.InputTokens)
	}
	if turn.Usage.OutputTokens == nil || *turn.Usage.OutputTokens != 17 {
		t.Errorf("OutputTokens = %v, want 17 (completion + reasoning)", turn.Usage.OutputTokens)
	}
	if turn.Model == nil || *turn.Model != "gpt-oss-120b" {
		t.Errorf("Model = %v, want gpt-oss-120b (from the persisted turn row)", turn.Model)
	}
}

// TestBuildTurnUsageNilWhenAbsent covers a DAG-only turn: the orchestrator itself
// recorded no tokens (all the work happened in gated nodes, surfaced separately
// via DagNodeState) — Turn.usage must stay nil, not a zero-valued struct, so the
// frontend can tell "no data" from "genuinely zero usage".
func TestBuildTurnUsageNilWhenAbsent(t *testing.T) {
	tc := store.TurnContent{ID: "t2", CreatedAt: time.Now(), UserText: "research X", AsstText: "The vetted answer."}

	turn := buildTurn(tc)

	if turn.Usage != nil {
		t.Errorf("Usage = %+v, want nil", turn.Usage)
	}
	if turn.Model != nil {
		t.Errorf("Model = %v, want nil (DAG turns carry no orchestrator model)", turn.Model)
	}
}
