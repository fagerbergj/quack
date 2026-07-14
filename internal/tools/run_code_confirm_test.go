package tools

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// The confirmation is on the SCRIPT. run_code is one tool call, so it pauses the
// node for a human exactly like any other confirm-tier tool — and what the human
// sees is a readable PROGRAM, approved once, before a single line of it runs. These
// tests drive that through the real dag executor, the real guard ladder and the real
// (jailed) filesystem tools: nothing here is a stand-in.
//
// The script is deliberately NOT idempotent: it increments a counter file. So the
// tests can assert not just "the side effect happened" but "the script body ran
// EXACTLY ONCE" — the re-execution hazard that made #219 exclude confirm-tier tools
// from the script API in the first place is the thing being ruled out.
const confirmScript = `
	let n = 0;
	try { n = parseInt(read_file({ path: "count.txt" }).content, 10); } catch (e) { n = 0; }
	write_file({ path: "count.txt", content: String(n + 1) });
	delete_path({ path: "doomed.txt" });
	return "done";
`

// scriptStub drives the worker + the vetting judge:
//   - judge requests (submit_verdict present) always pass;
//   - once run_code's RESOLVED response is in history (the script really ran) → final answer;
//   - a post-decision prompt saying DENIED → answer without the script;
//   - otherwise → propose the script.
type scriptStub struct{}

func (*scriptStub) Name() string { return "scriptStub" }

func (s *scriptStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if reqHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if reqHasResolvedResponse(req, vetting.RunCodeToolName) {
			yield(atText("FINAL: the script ran."), nil)
			return
		}
		if strings.Contains(atAllText(req), "DENIED") {
			yield(atText("FINAL: completed without the script."), nil)
			return
		}
		yield(atCall(vetting.RunCodeToolName, map[string]any{"code": confirmScript}), nil)
	}
}

// newScriptConfirmHarness builds a one-node plan whose worker holds REAL tools
// (Build, with the guard tiers the shipped config uses for delete_path) plus
// run_code over them, executed by the REAL dag executor.
func newScriptConfirmHarness(t *testing.T) (*dag.Executor, dag.Plan, *workspace.Jail) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	built, err := Build([]string{"read_file", "write_file", "delete_path", vetting.RunCodeToolName}, Deps{
		Workspace:       j,
		WorkspaceUserID: "u1",
		Sessions:        sessions,
		Guards:          map[string]string{"delete_path": "confirm"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	stub := &scriptStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Do the work.",
		Tools: built,
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": worker}, nil,
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := dag.Plan{ID: "t", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan, j
}

// nodePath resolves a path inside the node's own workspace — where a tool call made
// by node n1 of chat s actually lands.
func nodePath(t *testing.T, j *workspace.Jail, rel string) string {
	t.Helper()
	p, err := j.Resolve("u1", "s", filepath.Join(workspace.NodeDir("n1"), rel))
	if err != nil {
		t.Fatalf("resolve %q: %v", rel, err)
	}
	return p
}

func seedNodeFile(t *testing.T, j *workspace.Jail, rel, content string) {
	t.Helper()
	p := nodePath(t, j, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestRunCodeConfirmsTheScriptBeforeItRuns is the whole feature.
//
// Run 1: the worker proposes a script; the node PAUSES with the program in the
// message, and NOT ONE LINE of it has run — no file written, nothing deleted.
// Run 2: the human approves the program; it runs, ONCE, start to finish — including
// the delete_path call, which is confirm-tier and would have paused for a human on
// its own. It does not pause again: the human already approved this program, and
// approving a program means approving what it does.
func TestRunCodeConfirmsTheScriptBeforeItRuns(t *testing.T) {
	ex, plan, j := newScriptConfirmHarness(t)
	seedNodeFile(t, j, "doomed.txt", "delete me")
	count, doomed := nodePath(t, j, "count.txt"), nodePath(t, j, "doomed.txt")

	out1, paused, pauseID, pauseMsg := runConfirmTurn(t, ex, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused || out1["n1"] != "" {
		t.Fatalf("run1: want a pause with no output, got paused=%v out=%q", paused, out1["n1"])
	}
	if pauseID != "confirm-n1-r1" {
		t.Fatalf("run1: interrupt = %q, want confirm-n1-r1", pauseID)
	}
	if !strings.Contains(pauseMsg, vetting.RunCodeToolName) {
		t.Errorf("run1: the pause message does not name the operation being approved: %q", pauseMsg)
	}
	// THE POINT: the confirmation happens BEFORE the program runs. A script whose
	// first statements write and delete has done neither while it awaits approval.
	if exists(t, count) {
		t.Fatal("run1: the script WROTE a file while its confirmation was still pending — the approval must " +
			"come before the program runs, not after some of it has")
	}
	if !exists(t, doomed) {
		t.Fatal("run1: the script DELETED a file while its confirmation was still pending")
	}

	out2, paused2, _, _ := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID, "approve"), []string{"n1"})
	if paused2 {
		t.Fatal("run2: the node paused AGAIN — an approved program must not stop to re-ask about its own calls")
	}
	if !strings.Contains(out2["n1"], "the script ran") {
		t.Errorf("run2: out = %q, want the post-execution answer", out2["n1"])
	}
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("run2: the approved script did not write its file: %v", err)
	}
	// Exactly once. Not zero (it ran), not twice (the approval did not re-run it).
	if string(b) != "1" {
		t.Errorf("count.txt = %q, want \"1\" — the script body must execute exactly once, and only after approval", b)
	}
	// The in-script call to a CONFIRM-tier tool executed, on the strength of the
	// script's own approval. That is the capability #219 did not have.
	if exists(t, doomed) {
		t.Error("the in-script delete_path (confirm tier) did not run — an approved script's calls must not be " +
			"individually re-gated, or the script can do nothing useful")
	}
}

// TestRunCodeDeniedScriptNeverRuns: a denied program does not run AT ALL — no
// partial side effects — and the denial comes back as run_code's own result, so the
// model can carry on without it.
func TestRunCodeDeniedScriptNeverRuns(t *testing.T) {
	ex, plan, j := newScriptConfirmHarness(t)
	seedNodeFile(t, j, "doomed.txt", "delete me")
	count, doomed := nodePath(t, j, "count.txt"), nodePath(t, j, "doomed.txt")

	_, paused, pauseID, _ := runConfirmTurn(t, ex, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused {
		t.Fatal("run1: expected a pause")
	}

	out2, paused2, _, _ := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID, "deny"), []string{"n1"})
	if paused2 {
		t.Fatal("run2: still paused after the denial")
	}
	if !strings.Contains(out2["n1"], "without the script") {
		t.Errorf("run2: out = %q, want the without-the-script answer", out2["n1"])
	}
	if exists(t, count) {
		t.Error("a DENIED script wrote a file — a denied program must never execute, not even partly")
	}
	if !exists(t, doomed) {
		t.Error("a DENIED script deleted a file — a denied program must never execute, not even partly")
	}
}

// TestSafetyJudgeSeesTheScript: the judge tier reviews a PROGRAM now, so the
// program has to be in the prompt — as readable code, not as a JSON-escaped blob —
// and the judge has to be told what a script can and cannot do. This repo has been
// burned by a safety-judge prompt that asserted walls which did not exist (#206), so
// the claims here are exactly the true ones: the script has no capability of its own
// beyond the bound tools, and those calls carry the same jail and caps a direct call
// does.
func TestSafetyJudgeSeesTheScript(t *testing.T) {
	p := buildSafetyJudgePrompt("ship the fix", "run the tests", vetting.RunCodeToolName,
		map[string]any{"code": "const hits = grep({ pattern: \"func Build\" });\nreturn hits.matches.length;"}, "")
	for _, want := range []string{"ship the fix", "run the tests", "func Build", "hits.matches.length"} {
		if !strings.Contains(p, want) {
			t.Errorf("the safety judge's prompt does not carry %q — it cannot judge a program it cannot read:\n%s", want, p)
		}
	}
	if strings.Contains(p, `\"func Build\"`) {
		t.Error("the script reached the judge JSON-escaped; it must be rendered as code")
	}
	for _, want := range []string{"whole program", "no capability of its own", "loops"} {
		if !strings.Contains(safetyJudgeInstruction, want) {
			t.Errorf("the safety-judge instruction does not tell it how to judge a script (missing %q)", want)
		}
	}
}
