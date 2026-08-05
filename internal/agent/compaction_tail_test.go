package agent

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// A single colossal tool result must never reach the provider.
//
// Live failure: an unbounded grep match (48 MB in one session event) sailed
// through every rung of the compaction ladder because the retained tail
// admits its most recent content unconditionally, then hit the provider well
// over the context window - a fatal 400, unlike a truncated result the model
// can just re-run narrower.
func TestOversizedToolResultInTailIsClampedNotSent(t *testing.T) {
	const budget = 45_536

	// The monster: one grep result far bigger than the whole context window.
	huge := strings.Repeat("games_repo/.next/build/chunks/runtime.js.map:5: var x=1;\n", 200_000)
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "explore the goose repo"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "grep"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "c1", Name: "grep", Response: map[string]any{"matches": huge},
		}}}},
	}

	if got := estimateTokens(contents); got <= budget {
		t.Fatalf("test is not exercising the bug: contents estimate %d already fits the %d budget", got, budget)
	}

	clamped := clampToolResults(contents[1:], toolOutputMaxChars)
	if clamped != 1 {
		t.Fatalf("clamped %d tool results, want 1 (the 48 MB grep)", clamped)
	}
	if got := estimateTokens(contents); got > budget {
		t.Fatalf("after clamping, the request is still %d tokens against a %d budget - it would still 400", got, budget)
	}

	// The call/response pairing must survive: a response dropped out from under a live
	// call 400s just as hard as an oversized one.
	fr := contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.ID != "c1" || fr.Name != "grep" {
		t.Fatalf("the clamped response lost its identity (id/name); the call is now dangling: %+v", fr)
	}
	if fr.Response["truncated"] != true {
		t.Error("the clamped result must be loudly marked truncated, or the model trusts a partial answer as complete")
	}
	note, _ := fr.Response["note"].(string)
	if !strings.Contains(note, "narrower") {
		t.Errorf("the note must steer the model to a narrower query, else it re-runs the same grep and flails; got %q", note)
	}
}

// A tool result that already fits is left completely alone (the common case).
func TestClampLeavesNormalToolResultsUntouched(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "c1", Name: "read_file", Response: map[string]any{"content": "package main"},
		}}}},
	}
	if n := clampToolResults(contents, toolOutputMaxChars); n != 0 {
		t.Fatalf("clamped %d results; a small one must be untouched", n)
	}
	if got := contents[0].Parts[0].FunctionResponse.Response["content"]; got != "package main" {
		t.Fatalf("a fitting tool result was rewritten: %v", got)
	}
}
