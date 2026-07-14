package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// Code mode's whole product is that the bulk stays OUT of the model's context.
// The `calls` ledger elides it (TestScriptReadsThreeFilesAndReturnsOneResult) —
// but the model can hand the very same bytes back to ITSELF through the script's
// RETURN VALUE, and on code mode's first live run (2026-07-13) it did exactly
// that:
//
//	out[f] = { total_lines: content.total_lines, content: content.content };
//
// ~50 KB of source, straight back into the context the feature exists to protect,
// capped only by runCodeMaxResult. Nothing told it otherwise, so it would have
// done it again on the next turn. Detect it and SAY SO, in the result.
func TestReturningFileContentsIsWarned(t *testing.T) {
	tools, b := codeModeTools(t, "read_file")
	body := strings.Repeat("package main // a line of real source\n", 400) // ~15 KB
	writeUserFile(t, b, "big.go", body)

	out := runScriptTool(t, tools, `
		const c = read_file({ path: "big.go" });
		return { path: "big.go", total_lines: c.total_lines, content: c.content };
	`)
	if out.Error != "" {
		t.Fatalf("script failed: %s", out.Error)
	}
	if out.Warning == "" {
		t.Fatal("a script that returned a file's contents got no warning — the model has no way to learn " +
			"it defeated the feature, so it does it again next turn")
	}
	// The warning must name the SIZE — a vague scolding teaches nothing.
	if !strings.ContainsAny(out.Warning, "0123456789") {
		t.Errorf("warning does not name the size of what was returned: %q", out.Warning)
	}
	// And it must be actionable: say what to do instead.
	if !strings.Contains(strings.ToLower(out.Warning), "return") {
		t.Errorf("warning is not actionable: %q", out.Warning)
	}
	// The ledger's elision (#219) still holds — the warning is an ADDITION.
	rec, err := json.Marshal(out.Calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rec), "a line of real source") {
		t.Errorf("the calls ledger stopped eliding the payload: %s", rec)
	}
}

// The legitimate case must stay silent: a script that returns the STRUCTURE it
// derived (what the feature is for) is not warned at, however much it read.
func TestReturningASummaryIsNotWarned(t *testing.T) {
	tools, b := codeModeTools(t, "read_file")
	body := strings.Repeat("package main // a line of real source\n", 400)
	writeUserFile(t, b, "big.go", body)

	out := runScriptTool(t, tools, `
		const c = read_file({ path: "big.go" });
		return { path: "big.go", total_lines: c.total_lines, imports: c.content.split("\n").length };
	`)
	if out.Error != "" {
		t.Fatalf("script failed: %s", out.Error)
	}
	if out.Warning != "" {
		t.Errorf("a script that returned a summary was warned at: %q", out.Warning)
	}
}

// A script that COMPUTES a large answer — a generated file, a rendered report —
// is not echoing anything it read, and must not be warned at either. The check is
// "did you hand back the bytes the tools gave you", not "is your answer big".
func TestAComputedLargeAnswerIsNotWarned(t *testing.T) {
	tools, b := codeModeTools(t, "read_file")
	writeUserFile(t, b, "big.go", strings.Repeat("package main // a line of real source\n", 400))

	out := runScriptTool(t, tools, `
		const c = read_file({ path: "big.go" });
		let generated = "";
		for (let i = 0; i < 400; i++) { generated += "generated line " + i + "\n"; }
		return { lines_read: c.total_lines, generated: generated };
	`)
	if out.Error != "" {
		t.Fatalf("script failed: %s", out.Error)
	}
	if out.Warning != "" {
		t.Errorf("a script that computed its own large answer was warned at: %q", out.Warning)
	}
}

// The description must teach it BEFORE the model writes the script, not only
// after: the contrast (a structure is right, the contents are the mistake) has to
// be prominent, not buried in a Limits line.
func TestDescriptionForbidsReturningFileContents(t *testing.T) {
	desc := runCodePreamble()
	for _, want := range []string{"NEVER", "content"} {
		if !strings.Contains(desc, want) {
			t.Errorf("run_code's description does not prominently forbid returning file contents (missing %q)", want)
		}
	}
}
