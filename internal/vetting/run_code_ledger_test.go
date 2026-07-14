package vetting

import (
	"reflect"
	"strings"
	"testing"
)

// THE LEDGER GUARANTEE. Code mode's one real hazard is that a tool called from
// inside a script emits no session event: without the expansion in
// activityFromSessionAt, a script that wrote files and committed them would be
// INVISIBLE to the trust gate — the node failed for claiming work with no
// evidence, or, far worse, real work passing unverified.
//
// These tests pin the property that closes it: a call made from inside a script
// and the same call made directly must produce THE SAME activity. Not similar —
// the same. Every assertion below compares the two side by side rather than
// against a hand-written expectation, so the two paths cannot drift apart
// without a failure here.

// runCodeResp builds a run_code FunctionResponse carrying the compact record of
// the calls a script made — the shape internal/tools/run_code.go returns.
func runCodeResp(calls ...map[string]any) map[string]any {
	list := make([]any, 0, len(calls))
	for _, c := range calls {
		list = append(list, c)
	}
	return map[string]any{"result": `{"ok":true}`, "calls": list}
}

func innerCallRec(name string, args, result map[string]any) map[string]any {
	return map[string]any{"name": name, "args": args, "result": result}
}

// TestScriptWriteIsIndistinguishableFromDirectWrite is the headline test: the
// gate must not be able to tell a file written from inside a script from one
// written by a direct write_file call.
func TestScriptWriteIsIndistinguishableFromDirectWrite(t *testing.T) {
	const nodeDir = "node-1"
	args := map[string]any{"path": "repo/main.go", "content": "package main"}
	resp := map[string]any{"bytes": float64(12), "created": true}

	direct := activityFromSessionAt(newTestSession(t,
		fnCall("c1", "write_file", args),
		fnResp("c1", "write_file", resp),
	), nodeDir)

	scripted := activityFromSessionAt(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": `write_file({path: "repo/main.go", content: "package main"}); return "done";`}),
		fnResp("c1", RunCodeToolName, runCodeResp(innerCallRec("write_file", args, resp))),
	), nodeDir)

	if len(scripted.workspace) != 1 {
		t.Fatalf("scripted workspace ops = %d, want 1 — the script's write_file is INVISIBLE to the gate", len(scripted.workspace))
	}
	if got, want := scripted.workspace[0].detail, direct.workspace[0].detail; got != want {
		t.Errorf("ledger detail:\n scripted = %q\n direct   = %q\nthe two paths must record identically", got, want)
	}
	if !reflect.DeepEqual(scripted.written, direct.written) {
		t.Errorf("written: scripted = %v, direct = %v — the judge re-reads these to check the real post-edit source", scripted.written, direct.written)
	}
	if !reflect.DeepEqual(scripted.paths, direct.paths) {
		t.Errorf("paths: scripted = %v, direct = %v — citationScore backs a cited path only if it appears here", scripted.paths, direct.paths)
	}
	// And concretely, not just "equal to whatever direct produced":
	if !scripted.paths["repo/main.go"] {
		t.Error("paths is missing repo/main.go")
	}
	if len(scripted.written) != 1 || scripted.written[0] != nodeDir+"/repo/main.go" {
		t.Errorf("written = %v, want the node-relative path re-rooted under %q", scripted.written, nodeDir)
	}
}

// TestScriptCommitSeenByDeliveryCheck: the delivery check (delivery.go) fails a
// node that was asked to commit and did not. A commit made from inside a script
// is a real commit and must satisfy it.
func TestScriptCommitSeenByDeliveryCheck(t *testing.T) {
	args := map[string]any{"dir": "repo", "message": "fix: the bug"}
	resp := map[string]any{"sha": "abc123", "files_changed": float64(2)}

	direct := activityFromSession(newTestSession(t,
		fnCall("c1", "git_commit", args),
		fnResp("c1", "git_commit", resp),
	))
	scripted := activityFromSession(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": "git_commit(...)"}),
		fnResp("c1", RunCodeToolName, runCodeResp(innerCallRec("git_commit", args, resp))),
	))

	if !scripted.committed {
		t.Error("committed = false for a commit made inside a script — the delivery check would fail a node that really did commit")
	}
	if scripted.committed != direct.committed {
		t.Error("committed differs between the scripted and direct paths")
	}
	if got, want := scripted.workspace[0].detail, direct.workspace[0].detail; got != want {
		t.Errorf("ledger detail:\n scripted = %q\n direct   = %q", got, want)
	}
	// The SHA is the claim the judge checks the answer against.
	if !strings.Contains(scripted.workspace[0].detail, `sha="abc123"`) {
		t.Errorf("ledger detail %q must carry the commit sha", scripted.workspace[0].detail)
	}
}

// TestScriptGroundingCapture: the rest of the structured capture — a clone's
// dirs and repo URLs (which back citations), a push, a run_command (which the
// delivery check reads) — must survive the expansion too, or a node that did its
// work in a script would be judged as if it had done nothing.
func TestScriptGroundingCapture(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": "…"}),
		fnResp("c1", RunCodeToolName, runCodeResp(
			innerCallRec("git_clone",
				map[string]any{"url": "https://github.com/x/y", "dir": "y"},
				map[string]any{"dir": "y", "head": "def456", "default_branch": "main"}),
			// Note the SHAPE of a read made inside a script: the content is elided to
			// its size (internal/tools/run_code.go's compactResult), because putting
			// it back in the model's context is the very thing code mode prevents. The
			// gate still learns the file was read — which is what backs a citation of
			// it — but has no content sample to spot-check a quote against. That is
			// code mode's honest cost, and it is why an in-script read is a weaker
			// grounding signal than a direct one. See docs/code-mode.md.
			innerCallRec("read_file",
				map[string]any{"path": "y/README.md"},
				map[string]any{"content_chars": float64(19), "total_lines": float64(1)}),
			innerCallRec("run_command",
				map[string]any{"dir": "y", "command": "go test ./..."},
				map[string]any{"exit_code": float64(0)}),
			innerCallRec("git_push",
				map[string]any{"dir": "y"},
				map[string]any{"remote": "origin", "branch": "main", "sha": "def456"}),
		)),
	))

	if len(act.workspace) != 4 {
		t.Fatalf("workspace ops = %d, want 4 (one per in-script call, in order)", len(act.workspace))
	}
	if !act.ranCommand {
		t.Error("ranCommand = false: an in-script run_command must satisfy the execution check")
	}
	if !act.pushed {
		t.Error("pushed = false: an in-script git_push must satisfy the delivery check")
	}
	if len(act.clonedRepos) != 1 || act.clonedRepos[0] != "https://github.com/x/y" {
		t.Errorf("clonedRepos = %v: an in-script clone must back citations of that repo", act.clonedRepos)
	}
	if len(act.clonedDirs) != 1 || act.clonedDirs[0] != "y" {
		t.Errorf("clonedDirs = %v", act.clonedDirs)
	}
	if !act.paths["y/README.md"] {
		t.Error("an in-script read_file must back a citation of the file it read")
	}
	// An in-script read carries no content sample (see above) — the ledger records
	// THAT it happened, not what it returned. Pinned so the trade-off is a
	// decision, not an accident.
	if act.workspace[1].sample != "" {
		t.Errorf("read_file sample = %q, but an in-script read elides its content by design", act.workspace[1].sample)
	}
	if !strings.Contains(act.workspace[1].detail, "y/README.md") {
		t.Errorf("detail = %q, want the read recorded with its path", act.workspace[1].detail)
	}
	// Order is the script's order, which is what a reader of the ledger assumes.
	if act.workspace[0].tool != "git_clone" || act.workspace[3].tool != "git_push" {
		t.Errorf("ledger order = %v, want the order the script called them in", []string{act.workspace[0].tool, act.workspace[3].tool})
	}
}

// TestScriptFailedCallRecorded: a call that FAILED inside a script stays in the
// ledger, marked. "The tests passed" claimed over a failed in-script run must be
// as contradictable as it is for a direct call.
func TestScriptFailedCallRecorded(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": "…"}),
		fnResp("c1", RunCodeToolName, runCodeResp(
			innerCallRec("run_command",
				map[string]any{"dir": "y", "command": "go test ./..."},
				map[string]any{"error": "exit status 1"}),
		)),
	))
	if len(act.workspace) != 1 {
		t.Fatalf("workspace ops = %d, want the FAILED call recorded, not dropped", len(act.workspace))
	}
	if !strings.Contains(act.workspace[0].detail, "FAILED") {
		t.Errorf("detail = %q, want it marked FAILED", act.workspace[0].detail)
	}
	if act.ranCommand {
		t.Error("ranCommand = true for a FAILED run_command: a failed run grounds nothing")
	}
}

// TestScriptCdTracksCwdForLaterWrites: a `cd` inside a script moves the session
// cwd for real, so the paths of writes AFTER it — in the same script or in a
// later direct call — must resolve against it. Getting this wrong makes the
// judge re-read the wrong file and silently degrade to trusting the answer's
// self-report (the regression that has bitten twice; see writtenRel).
func TestScriptCdTracksCwdForLaterWrites(t *testing.T) {
	const nodeDir = "node-1"
	act := activityFromSessionAt(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": "…"}),
		fnResp("c1", RunCodeToolName, runCodeResp(
			innerCallRec("cd", map[string]any{"dir": "repo"}, map[string]any{"dir": "repo", "cwd": "repo"}),
			innerCallRec("write_file", map[string]any{"path": "main.go"}, map[string]any{"bytes": float64(3), "created": true}),
		)),
		// A DIRECT call after the script sees the cwd the script's cd left behind —
		// the tool wrote it to session state, so the scanner must carry it across.
		fnCall("c2", "write_file", map[string]any{"path": "other.go"}),
		fnResp("c2", "write_file", map[string]any{"bytes": float64(3), "created": true}),
	), nodeDir)

	want := []string{nodeDir + "/repo/main.go", nodeDir + "/repo/other.go"}
	if !reflect.DeepEqual(act.written, want) {
		t.Errorf("written = %v, want %v — an in-script cd must move the cwd later writes resolve against", act.written, want)
	}
}

// TestRunCodeItselfIsNotALedgerOp: run_code is a vehicle, not an operation. Only
// what the script actually DID belongs in the ledger; recording the wrapper as
// well would double-count and hand the judge a "run_code(...)" line it cannot
// check any claim against.
func TestRunCodeItselfIsNotALedgerOp(t *testing.T) {
	if isWorkspaceTool(RunCodeToolName) {
		t.Fatal("run_code must not be a workspace tool in its own right")
	}
	act := activityFromSession(newTestSession(t,
		fnCall("c1", RunCodeToolName, map[string]any{"code": "return 1;"}),
		fnResp("c1", RunCodeToolName, runCodeResp()),
	))
	if len(act.workspace) != 0 {
		t.Errorf("workspace ops = %v, want none: the script touched nothing", act.workspace)
	}
}

// TestExpandRunCodeTolerantOfMalformedRecord: a response with no record, or a
// junk one (a pre-feature event replayed out of the durable event log), expands
// to nothing rather than panicking. No evidence is the safe direction: an
// unevidenced claim fails.
func TestExpandRunCodeTolerantOfMalformedRecord(t *testing.T) {
	for name, resp := range map[string]map[string]any{
		"absent":     {"result": "1"},
		"nil":        {"calls": nil},
		"not a list": {"calls": "nope"},
		"junk entry": {"calls": []any{"nope", 3, map[string]any{"no_name": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := expandRunCode(resp); len(got) != 0 {
				t.Errorf("expandRunCode(%v) = %v, want nothing", resp, got)
			}
		})
	}
}
