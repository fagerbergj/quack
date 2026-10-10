package vetting

import (
	"strings"
	"testing"
)

// A read-only node must be tested for completion against its own task, not the whole worker prompt that
// carries the user's request: an explorer without commit/push tools never finished otherwise.
func TestReadOnlyNodeIsNotHeldToTheUserRequestsDelivery(t *testing.T) {
	// The node's own read-only task, as cfg.Task carries it.
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

	if workIncomplete(answer, explorerTask, act, false, true, false, false) {
		t.Fatal("an explorer that produced its report is being called incomplete against its OWN task")
	}

	// Judged against the assembled prompt, the explorer inherits the user's delivery demand and can never finish.
	if !workIncomplete(answer, assembledPrompt, act, false, true, false, false) {
		t.Fatal("assembled prompt no longer reads as implement-and-deliver - this oracle can no longer detect the regression it exists to catch")
	}
	// ...so the gate's completion test must take cfg.Task, the node's own task, never the prompt.
}

// An implementer whose own task demands delivery must still be held to it.
func TestImplementerIsStillHeldToItsDelivery(t *testing.T) {
	const implementerTask = "Implement code mode in quack with tests, run the repo's checks, " +
		"commit on a branch named feat/code-mode, push it, and open a pull request."

	answer := "I implemented code mode. Here is the design…"

	if !workIncomplete(answer, implementerTask, workerActivity{}, false, true, false, false) {
		t.Fatal("an implementer that committed and pushed NOTHING was accepted as finished")
	}
	if !workIncomplete(answer, implementerTask, workerActivity{committed: true}, false, true, false, false) {
		t.Fatal("an implementer that committed but never pushed was accepted as finished")
	}
}

// The judge must score a node against its own task too: judged against the full worker prompt, a read-only
// explorer fails for "commit, push, open a PR" work that was never its to do.
func TestJudgeIsScopedToTheNodesOwnTask(t *testing.T) {
	const explorerTask = "Clone goose and read how it exposes tools. Cite the files you read."
	const fullPrompt = "BACKGROUND - the user's full request.\nImplement code mode in quack. " +
		"Commit on a branch, push it, and open a pull request.\n\n---\n\nYOUR TASK:\n" + explorerTask

	got := buildJudgePrompt("", "rubric", explorerTask, "", questionContent(fullPrompt), "goose uses ExtensionManager…", "", workerActivity{}, "")

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
