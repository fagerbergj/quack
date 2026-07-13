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

// TestSafetyJudgeInstructionCalibration pins the system prompt's load-bearing
// content in BOTH directions.
//
// The prompt used to tell the judge that "every path is confined to the agent's
// workspace jail" and that "commands run argv-only with no shell", then told it
// not to re-litigate the sandbox. Neither claim was ever true of a run_command
// CHILD process (its arguments are path-checked by nothing; `sh -c "…"` trips no
// metachar), so the one automated gate we have was calibrated to stand down on
// exactly the class of operation it exists to catch. Those claims must never
// come back — hence the must-NOT-contain half.
//
// The anti-over-denial calibration (live usage 2026-07-10: the judge denied
// anything destructive-LOOKING) is kept, but only for what IS genuinely
// confined: the task's own artifacts inside its own repo.
func TestSafetyJudgeInstructionCalibration(t *testing.T) {
	for _, want := range []string{
		// What actually holds — stated as narrowly as it is true.
		"resolve every path inside the agent's workspace jail",
		"run_command is DIFFERENT",
		"real operating-system process",
		`"No shell" is a habit guard, not a wall`,
		// The deny grounds that the (false) wall claims used to suppress.
		"DENY:",
		"INLINE code",
		"python -c",
		"credentials or dotfile config",
		".ssh",
		"outside the task's own repository",
		"piping remote content into an interpreter",
		"external destination",
		"scope escalation",
		// Anti-over-denial calibration, scoped to what IS confined.
		"ALLOW",
		"rm -rf node_modules",
		"DENY examples: git_push when the task is read-only research",
	} {
		if !strings.Contains(safetyJudgeInstruction, want) {
			t.Errorf("safetyJudgeInstruction missing calibration anchor %q", want)
		}
	}
	// Claims that are FALSE for a run_command child. Telling the judge these
	// walls hold is how it gets talked out of denying `sh -c cat ~/.ssh/id_rsa`.
	for _, forbidden := range []string{
		"You are NOT the sandbox",
		"argv-only with no shell",
		"Deterministic walls already hold",
		"could be dangerous in general",
	} {
		if strings.Contains(safetyJudgeInstruction, forbidden) {
			t.Errorf("safetyJudgeInstruction claims a wall that does not exist for a run_command child: %q", forbidden)
		}
	}
}
