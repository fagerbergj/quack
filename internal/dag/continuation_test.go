package dag

import (
	"context"
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

func TestResolveContinue_NoContinueField(t *testing.T) {
	c := resolveContinue(context.Background(), Node{ID: "n2"}, vetting.Config{}, nil, "chat", "plan2")
	if c.ok || c.fallbackReason != "" {
		t.Fatalf("no Continue field: want zero-value continuation, got %+v", c)
	}
}

func TestResolveContinue_NoLookupWired(t *testing.T) {
	c := resolveContinue(context.Background(), Node{ID: "n2", Continue: "n1"}, vetting.Config{}, nil, "chat", "plan2")
	if c.ok || c.fallbackReason == "" {
		t.Fatalf("nil lookup: want a fallback reason, got %+v", c)
	}
}

func TestResolveContinue_NotFound(t *testing.T) {
	lookup := func(context.Context, string, string, string) (PriorNode, bool, error) { return PriorNode{}, false, nil }
	c := resolveContinue(context.Background(), Node{ID: "n2", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
	if c.ok || c.fallbackReason == "" {
		t.Fatalf("not found: want a fallback reason, got %+v", c)
	}
}

func TestResolveContinue_LookupError(t *testing.T) {
	lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
		return PriorNode{}, false, errors.New("db down")
	}
	c := resolveContinue(context.Background(), Node{ID: "n2", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
	if c.ok || c.fallbackReason == "" {
		t.Fatalf("lookup error: want a fallback reason, got %+v", c)
	}
}

func TestResolveContinue_NonTerminalRejected(t *testing.T) {
	for _, st := range []NodeStatus{StatusQueued, StatusRunning, StatusPaused, StatusNeedsInput} {
		lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
			return PriorNode{Status: st, Handle: SessionHandle{Agent: "coder"}}, true, nil
		}
		c := resolveContinue(context.Background(), Node{ID: "n2", AgentName: "coder", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
		if c.ok {
			t.Errorf("status %s: want rejected as non-terminal, got ok", st)
		}
	}
}

func TestResolveContinue_TerminalStatusesAccepted(t *testing.T) {
	for _, st := range []NodeStatus{StatusDone, StatusFailed, StatusCancelled} {
		lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
			return PriorNode{Status: st, Handle: SessionHandle{Agent: "coder", Scope: "n1"}}, true, nil
		}
		c := resolveContinue(context.Background(), Node{ID: "n2", AgentName: "coder", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
		if !c.ok {
			t.Errorf("status %s: want accepted, got fallback %q", st, c.fallbackReason)
		}
	}
}

func TestResolveContinue_AgentMismatchRejected(t *testing.T) {
	lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
		return PriorNode{Status: StatusDone, Handle: SessionHandle{Agent: "reviewer", Scope: "n1"}}, true, nil
	}
	c := resolveContinue(context.Background(), Node{ID: "n2", AgentName: "coder", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
	if c.ok {
		t.Fatal("agent mismatch (reviewer -> coder): want rejected, got ok")
	}
}

// TestResolveContinue_StaleHeadFallsBackFresh: the prior node recorded a
// HEAD sha but the live workspace (cfg.Setup == nil here, so CloneHeadSHA
// returns "") no longer matches it - the freshness check must reject.
func TestResolveContinue_StaleHeadFallsBackFresh(t *testing.T) {
	lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
		return PriorNode{Status: StatusDone, Handle: SessionHandle{Agent: "coder", Scope: "n1", HeadSHA: "deadbeef1234"}}, true, nil
	}
	c := resolveContinue(context.Background(), Node{ID: "n2", AgentName: "coder", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
	if c.ok {
		t.Fatal("stale head (recorded sha vs no live clone): want rejected, got ok")
	}
}

// TestResolveContinue_NoRepoBothSidesMatchTrivially: neither the prior node
// nor this node has a clone (HeadSHA == "" on both sides) - freshness is
// trivially satisfied, matching a plain conversation node.
func TestResolveContinue_NoRepoBothSidesMatchTrivially(t *testing.T) {
	lookup := func(context.Context, string, string, string) (PriorNode, bool, error) {
		return PriorNode{Status: StatusDone, Handle: SessionHandle{Agent: "coder", Scope: "n1"}, Output: "prior answer"}, true, nil
	}
	c := resolveContinue(context.Background(), Node{ID: "n2", AgentName: "coder", Continue: "n1"}, vetting.Config{}, lookup, "chat", "plan2")
	if !c.ok {
		t.Fatalf("no-repo match: want accepted, got fallback %q", c.fallbackReason)
	}
	if c.priorOutput != "prior answer" {
		t.Fatalf("priorOutput = %q, want %q", c.priorOutput, "prior answer")
	}
}

// TestBuildSessionHandle_ADK is TestNewGatedNode_CapturesADKBranchOnFinish
// (continue_wiring_test.go) - buildSessionHandle's ADK-kind Branch/
// IsolationScope capture needs a real adkagent.Context (ctx.Branch()/
// ctx.IsolationScope() are ADK-scheduler-assigned, not hand-mockable), so
// it's exercised end to end there rather than with a bare struct here.

func TestBuildSessionHandle_ACP(t *testing.T) {
	token := "plan1/n1-acp-handle-test"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)
	vetting.SetAdvisorThreadSessionID(token, "acp-sess-42")

	cfg := vetting.Config{ChatID: "chat1", NodeID: "n1", ExternalWorker: true}
	h := buildSessionHandle(nil, cfg, Node{ID: "n1", AgentName: "pi"}, token)
	if h.Kind != "acp" {
		t.Fatalf("Kind = %q, want acp", h.Kind)
	}
	if h.ID != "acp-sess-42" {
		t.Fatalf("ID = %q, want the ACP protocol session id", h.ID)
	}
}

func TestEncodeDecodeSessionHandle_RoundTrip(t *testing.T) {
	h := SessionHandle{Kind: "acp", ID: "s1", Agent: "coder", Scope: "n1", HeadSHA: "abc123"}
	got := DecodeSessionHandle(EncodeSessionHandle(h))
	if got != h {
		t.Fatalf("round trip = %+v, want %+v", got, h)
	}
	if EncodeSessionHandle(SessionHandle{}) != "" {
		t.Fatal("zero-value handle must encode to empty string")
	}
	if got := DecodeSessionHandle(""); got != (SessionHandle{}) {
		t.Fatalf("decoding empty string = %+v, want zero value", got)
	}
	if got := DecodeSessionHandle("not json"); got != (SessionHandle{}) {
		t.Fatalf("decoding garbage = %+v, want zero value (fail open)", got)
	}
}

// TestResumeADKContext_GuardsAgainstUniversalVisibility pins the one thing
// resumeADKContext must get right without a real ADK context: it must
// never hand back an overridden ctx for a case where the override would be
// empty-branch (which ADK treats as "sees everything", not isolation).
// The "actually overrides" case needs a real context - see
// TestNewGatedNode_CapturesADKBranchOnFinish (continue_wiring_test.go).
func TestResumeADKContext_GuardsAgainstUniversalVisibility(t *testing.T) {
	cases := []struct {
		name string
		c    continuation
	}{
		{"not ok", continuation{}},
		{"acp kind", continuation{ok: true, handle: SessionHandle{Kind: "acp", Branch: "n1"}}},
		{"adk kind, no captured branch", continuation{ok: true, handle: SessionHandle{Kind: "adk"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeADKContext(nil, tc.c); got != nil {
				t.Fatalf("resumeADKContext(nil, %+v) = %v, want nil (unchanged) ctx", tc.c, got)
			}
		})
	}
}
