package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func injectedTexts(req *model.LLMRequest) []string {
	var out []string
	for _, c := range req.Contents {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && strings.TrimSpace(p.Text) != "" {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

// #1029 review: the drain PEEKS, so the same text stays pending across every
// model call of a round - hence the dedupe. But keying it on the text alone
// swallowed a REPEATED steer: a user who sends "STOP", sees nothing, and sends
// "STOP" again got the second one dropped from the live path. An empty drain
// means the gate consumed the queue, so anything after it is genuinely new.
func TestSteerCallback_RedeliversTheSameTextAfterTheGateDrains(t *testing.T) {
	var pending string
	cb := steerCallback(func() string { return pending })

	// Round 1: the steer is pending and must land exactly once, however many
	// model calls the round makes.
	pending = "STOP"
	r1 := &model.LLMRequest{}
	cb(nil, r1)
	r2 := &model.LLMRequest{}
	cb(nil, r2)
	if got := injectedTexts(r1); len(got) != 1 || got[0] != "STOP" {
		t.Fatalf("first call injections = %v, want exactly one STOP", got)
	}
	if got := injectedTexts(r2); len(got) != 0 {
		t.Fatalf("second call in the same round injected %v; the steer is still pending, not new", got)
	}

	// The gate boundary drains it durably: the queue is now empty.
	pending = ""
	r3 := &model.LLMRequest{}
	cb(nil, r3)

	// The user repeats the SAME word because nothing appeared to happen.
	pending = "STOP"
	r4 := &model.LLMRequest{}
	cb(nil, r4)
	if got := injectedTexts(r4); len(got) != 1 || got[0] != "STOP" {
		t.Fatalf("a repeated steer after the gate drained the first was dropped; injections = %v", got)
	}
}

// A genuinely new steer arriving later in the same round must still land.
func TestSteerCallback_NewTextInTheSameRoundStillLands(t *testing.T) {
	var pending string
	cb := steerCallback(func() string { return pending })

	pending = "STOP"
	r1 := &model.LLMRequest{}
	cb(nil, r1)

	pending = "STOP\n\nAND SUMMARISE"
	r2 := &model.LLMRequest{}
	cb(nil, r2)
	if got := injectedTexts(r2); len(got) != 1 {
		t.Fatalf("a new steer in the same round must land; injections = %v", got)
	}
}
