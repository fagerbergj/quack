package vetting

import (
	"strings"
	"testing"
)

// A read-only node must not be held to a delivery its own task never asked for.
//
// THE LIVE FAILURE (code-mode dogfood, 2026-07-13). Every node's worker prompt carries
// the user's verbatim request as background (dag.buildTask). The request ended in
// "commit on a branch named feat/code-mode, push it, and open a pull request". The
// continuation loop tested completion against that whole PROMPT rather than the node's
// own task - so a `code-explorer`, whose task is "clone goose and read how it exposes
// tools" and which has NO commit or push tools at all, was judged incomplete on
//
//	committed=false pushed=false
//
// forever. It could never satisfy it. Every explorer ran the continuation loop to its
// bound, reading for HOURS; across an entire evening of runs not ONE explorer node ever
// finished or reached a judge round. The nodes weren't slow - they were unfinishable.
func TestReadOnlyNodeIsNotHeldToTheUserRequestsDelivery(t *testing.T) {
	// The node's OWN task: read-only. This is what cfg.Task carries.
	const explorerTask = "Clone https://github.com/aaif-goose/goose (shallow) and read the ACTUAL SOURCE " +
		"to understand how goose exposes tools/extensions to the model. Cite the files you read."

	// The assembled worker prompt: the node's task PLUS the user's verbatim request.
	const userRequest = "Implement \"code mode\" in the quack repository. PHASE 3 - IMPLEMENT IT. " +
		"Commit on a branch named exactly feat/code-mode, push it, and open a pull request."
	const assembledPrompt = "BACKGROUND - the user's full request, verbatim.\n" + userRequest +
		"\n\n---\n\nYOUR TASK - do this, and ONLY this:\n" + explorerTask

	// The explorer did its job: it read the source and wrote its report. It committed
	// nothing and pushed nothing, because it cannot and must not.
	act := workerActivity{}
	answer := "goose registers tools via ExtensionManager (crates/goose/src/agents/extension_manager.rs)…"

	if workIncomplete(answer, explorerTask, act, false, false) {
		t.Fatal("an explorer that produced its report is being called incomplete against its OWN task")
	}

	// The bug: judged against the assembled prompt, the explorer inherits the user's
	// delivery demand and can never finish.
	if !workIncomplete(answer, assembledPrompt, act, false, false) {
		t.Skip("the assembled prompt no longer reads as implement-and-deliver; this test can no longer detect the regression")
	}
	// ...which is precisely why the loop must never be given the prompt. Guard it:
	// the gate's completion test takes cfg.Task, and cfg.Task is the node's own task.
	// (See node.go's continuation loop - it used `prompt` and hung every explorer.)
}

// The implementer, whose own task DOES demand delivery, must still be held to it -
// this is the check that stopped workers from "finishing" with uncommitted code.
func TestImplementerIsStillHeldToItsDelivery(t *testing.T) {
	const implementerTask = "Implement code mode in quack with tests, run the repo's checks, " +
		"commit on a branch named feat/code-mode, push it, and open a pull request."

	answer := "I implemented code mode. Here is the design…"

	if !workIncomplete(answer, implementerTask, workerActivity{}, false, false) {
		t.Fatal("an implementer that committed and pushed NOTHING was accepted as finished")
	}
	if !workIncomplete(answer, implementerTask, workerActivity{committed: true}, false, false) {
		t.Fatal("an implementer that committed but never pushed was accepted as finished")
	}
}

// The JUDGE must score a node against its own task too - the same contamination, one
// stage later. It is handed the worker's full prompt as "the user's question", and that
// prompt carries the whole request as background. A judge scoring a read-only explorer
// against "commit, push, open a PR" fails it for work that was never its to do.
//
// No explorer had ever REACHED the judge (the continuation loop hung them all first), so
// this had never fired. It was the next wall.
func TestJudgeIsScopedToTheNodesOwnTask(t *testing.T) {
	const explorerTask = "Clone goose and read how it exposes tools. Cite the files you read."
	const fullPrompt = "BACKGROUND - the user's full request.\nImplement code mode in quack. " +
		"Commit on a branch, push it, and open a pull request.\n\n---\n\nYOUR TASK:\n" + explorerTask

	got := buildJudgePrompt("", "rubric", explorerTask, questionContent(fullPrompt), "goose uses ExtensionManager…", "", workerActivity{}, "")

	if !strings.Contains(got, "this node's own task") {
		t.Error("the judge is not told WHAT it is scoring; it will grade the explorer against the whole request")
	}
	if !strings.Contains(got, explorerTask) {
		t.Error("the node's own task is not in the judge prompt")
	}
	if !strings.Contains(got, "read-only research node that committed no code has not failed") {
		t.Error("the judge is not told that unassigned work is not this node's failure")
	}
}
