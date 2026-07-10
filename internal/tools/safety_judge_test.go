package tools

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
)

// safetyStub is a model.LLM that answers a safety-judge run by calling
// submit_safety_verdict with a fixed verdict — or with plain text (never
// calling the tool) when submit is false, to prove the no-verdict error path.
type safetyStub struct {
	allow  bool
	reason string
	submit bool
	// sawPrompt captures the user prompt for assertions.
	sawPrompt string
}

func (*safetyStub) Name() string { return "safetyStub" }

func (s *safetyStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.sawPrompt = atAllText(req)
		if !s.submit {
			yield(atText("I think this is fine."), nil)
			return
		}
		yield(atCall(submitSafetyVerdictTool, map[string]any{"allow": s.allow, "reason": s.reason}), nil)
	}
}

func TestSafetyJudgeAllowAndDeny(t *testing.T) {
	for _, c := range []struct {
		allow  bool
		reason string
	}{
		{true, "clearly on task"},
		{false, "unrelated to the task"},
	} {
		stub := &safetyStub{allow: c.allow, reason: c.reason, submit: true}
		judge := NewSafetyJudge(stub)
		allow, reason, err := judge(context.Background(), "fix the bug", "patch pkg X", "delete_path",
			map[string]any{"path": "repo", "recursive": true}, "  - read_file")
		if err != nil {
			t.Fatalf("judge: %v", err)
		}
		if allow != c.allow || reason != c.reason {
			t.Errorf("verdict = (%v, %q), want (%v, %q)", allow, reason, c.allow, c.reason)
		}
		// The one focused prompt carries every context section.
		for _, want := range []string{"fix the bug", "patch pkg X", "delete_path", "read_file"} {
			if !strings.Contains(stub.sawPrompt, want) {
				t.Errorf("judge prompt missing %q:\n%s", want, stub.sawPrompt)
			}
		}
	}
}

func TestSafetyJudgeNoVerdictIsError(t *testing.T) {
	stub := &safetyStub{submit: false}
	judge := NewSafetyJudge(stub)
	_, _, err := judge(context.Background(), "", "", "git_push", map[string]any{}, "")
	if err == nil {
		t.Fatal("expected an error when the judge never calls submit_safety_verdict (the guard then fails closed)")
	}
}
