package dag

import (
	"context"
	"errors"
	"strings"
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

func TestBuildSessionHandle_ADK(t *testing.T) {
	cfg := vetting.Config{ChatID: "chat1", NodeID: "n1"}
	h := buildSessionHandle(cfg, Node{ID: "n1", AgentName: "writer"}, "plan1/n1")
	if h.Kind != "adk" {
		t.Fatalf("Kind = %q, want adk", h.Kind)
	}
	if h.ID != "chat1" {
		t.Fatalf("ID = %q, want the chat id (ADK session id == chat id)", h.ID)
	}
	if h.Agent != "writer" || h.Scope != "n1" {
		t.Fatalf("handle = %+v", h)
	}
}

func TestBuildSessionHandle_ACP(t *testing.T) {
	token := "plan1/n1-acp-handle-test"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)
	vetting.SetAdvisorThreadSessionID(token, "acp-sess-42")

	cfg := vetting.Config{ChatID: "chat1", NodeID: "n1", ExternalWorker: true}
	h := buildSessionHandle(cfg, Node{ID: "n1", AgentName: "pi"}, token)
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

func TestContinuePreamble(t *testing.T) {
	if p := continuePreamble(continuation{}); p != "" {
		t.Fatalf("not-ok continuation: want empty preamble, got %q", p)
	}
	acp := continuation{ok: true, handle: SessionHandle{Kind: "acp"}, priorOutput: "prior"}
	if p := continuePreamble(acp); p != "" {
		t.Fatalf("acp kind: want no text preamble (session/load carries it), got %q", p)
	}
	adk := continuation{ok: true, handle: SessionHandle{Kind: "adk"}, priorOutput: "prior answer text"}
	p := continuePreamble(adk)
	if p == "" {
		t.Fatal("adk kind with prior output: want a non-empty preamble")
	}
	if !strings.Contains(p, "prior answer text") {
		t.Fatalf("preamble %q does not carry the prior output", p)
	}
}
