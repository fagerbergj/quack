package acp

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// #1048: recordUsage was fed only the shared stamp (a.coords), with no
// reference to the round's own ctx coords - unlike traced.go's field-by-field
// merge (#1047), a concurrent sibling round's stamp could steal this round's
// attribution.
func TestRound_CtxCoordsWinOverTheSharedStampForUsage(t *testing.T) {
	reader := newUsageTestMeter(t)
	a := usageTestAgent(t, "usage", nil)

	// A concurrent sibling round stamped last and is still "current" on this
	// shared agent instance.
	a.SetLedgerCoords(ledger.Coords{Agent: "sibling-agent", User: "sibling-user"})

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "my-agent"})
	if err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "add the feature", "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}

	points := tokenUsagePoints(t, reader)
	dp, ok := points[otelobs.GenAITokenTypeInput]
	if !ok {
		t.Fatal("no input token data point")
	}
	if got := attrVal(dp.Attributes, "agent"); got != "my-agent" {
		t.Errorf("agent = %q, want my-agent (ctx must win over the sibling's stamp)", got)
	}
	if got := attrVal(dp.Attributes, "user"); got != "sibling-user" {
		t.Errorf("user = %q, want sibling-user (blanks must still be filled from the stamp)", got)
	}
}
