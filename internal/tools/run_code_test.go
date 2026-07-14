package tools

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// codeModeTools builds a real, fully-wrapped tool set for a fresh temp jail,
// with run_code assembled over it exactly as production does — same Build, same
// guard ladder, same order. Nothing here is a stand-in for the real thing: the
// point of most of these tests is that a script's call IS a real tool call.
func codeModeTools(t *testing.T, names ...string) (map[string]tool.Tool, fsBinding) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	built, err := Build(append(names, vetting.RunCodeToolName), Deps{
		Workspace:       j,
		WorkspaceUserID: "u1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := map[string]tool.Tool{}
	for _, b := range built {
		byName[b.Name()] = b
	}
	if byName[vetting.RunCodeToolName] == nil {
		t.Fatal("Build did not produce run_code")
	}
	return byName, fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
}

// scriptCtx is cd_test's fakeCtx (a real State, so a script's cd persists) plus
// the one method functiontool's own Run consults on the way in.
type scriptCtx struct{ *fakeCtx }

func (scriptCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

func newScriptCtx() scriptCtx { return scriptCtx{newFakeCtx()} }

// runScriptTool invokes run_code the way the model does — through the tool's own
// Run — and decodes the single result that comes back.
func runScriptTool(t *testing.T, tools map[string]tool.Tool, code string) runCodeResult {
	t.Helper()
	raw, err := tools[vetting.RunCodeToolName].(runnableTool).Run(newScriptCtx(), map[string]any{"code": code})
	if err != nil {
		t.Fatalf("run_code returned a Go error (%v); every failure must come back INSIDE the result, or the calls the script already made are lost to the ledger", err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out runCodeResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. The API is GENERATED from the real declarations — there is no second source
// ---------------------------------------------------------------------------

// fakeToolArgs is a tool nobody has ever heard of. Its name and its parameters
// must show up in run_code's description without a single edit to run_code.go —
// that is the whole no-drift property.
type fakeToolArgs struct {
	Corpus  string   `json:"corpus"`
	Depth   int      `json:"depth,omitempty"`
	Filters []string `json:"filters,omitempty"`
}

type fakeToolResult struct {
	Verdict string `json:"verdict"`
}

func TestAPIIsGeneratedFromRealDeclarations(t *testing.T) {
	fake, err := functiontool.New[fakeToolArgs, fakeToolResult](
		functiontool.Config{Name: "divine_the_corpus", Description: "Divines a corpus.\n\nLong tail nobody needs in the listing."},
		func(_ adkagent.Context, a fakeToolArgs) (fakeToolResult, error) {
			return fakeToolResult{Verdict: a.Corpus}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := newRunCode([]tool.Tool{fake}, nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := rc.Description()

	// The name, every parameter, and the return field — all of them arrived from
	// the tool's own Declaration(), which ADK inferred from its Go argument struct.
	for _, want := range []string{
		"divine_the_corpus(",
		"corpus: string",
		"depth?: integer",
		"filters?: string[]",
		"verdict: string",
		"Divines a corpus.",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("run_code description is missing %q — the API is not being generated from the declaration.\n%s", want, desc)
		}
	}
	// Required-ness comes from the schema, not from a guess: corpus has no
	// omitempty, so it is required; depth does, so it is optional.
	if strings.Contains(desc, "corpus?:") {
		t.Error("corpus is required in the schema but the listing marks it optional")
	}
	// The listing summarizes; it does not paste each tool's whole description back
	// into the context (the model already holds the full one).
	if strings.Contains(desc, "Long tail nobody needs") {
		t.Error("the listing is reproducing whole tool descriptions; it should carry only the first paragraph")
	}
}

// TestNoHandMaintainedToolList is the drift guard the design demands: code mode
// must not contain a parallel, hand-written copy of the tool surface. It is
// enforced structurally — not one string literal in run_code.go may be the name
// of a registered tool. If someone ever "helpfully" special-cases read_file or
// git_commit in here, this fails.
func TestNoHandMaintainedToolList(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run_code.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if _, isTool := registry[val]; isTool {
			t.Errorf("run_code.go names the tool %q in a string literal at %s.\n"+
				"Code mode's API must be GENERATED from the tools' own declarations — a hand-maintained "+
				"list is exactly the thing that drifts.", val, fset.Position(lit.Pos()))
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// 2. Many calls, ONE result — and the bulk never reaches the model
// ---------------------------------------------------------------------------

func TestScriptReadsThreeFilesAndReturnsOneResult(t *testing.T) {
	tools, b := codeModeTools(t, "read_file", "write_file", "list_dir", "glob", "grep")
	secret := "SUPERSECRET-NEEDLE-DO-NOT-LEAK"
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		writeUserFile(t, b, f, secret+" in "+f+"\nline2\nline3\n")
	}

	out := runScriptTool(t, tools, `
		const files = ["a.txt", "b.txt", "c.txt"];
		let total = 0;
		for (const f of files) {
			total += read_file({ path: f }).content.length;
		}
		return { files: files.length, total_chars: total };
	`)

	if out.Error != "" {
		t.Fatalf("script failed: %s", out.Error)
	}
	// ONE result, from THREE reads.
	if len(out.Calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(out.Calls))
	}
	var got struct {
		Files      int `json:"files"`
		TotalChars int `json:"total_chars"`
	}
	if err := json.Unmarshal([]byte(out.Result), &got); err != nil {
		t.Fatalf("result %q: %v", out.Result, err)
	}
	if got.Files != 3 || got.TotalChars == 0 {
		t.Errorf("result = %+v, want the script's own computed summary", got)
	}

	// THE POINT OF THE FEATURE: the three files' contents are NOT in what comes
	// back. The script read them; the model never sees them.
	whole, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), secret) {
		t.Errorf("the file contents came back into the model's context — code mode is not eliding the payload:\n%s", whole)
	}
	// But the ledger's record of WHAT was read is intact.
	for i, c := range out.Calls {
		if c.Name != "read_file" {
			t.Errorf("calls[%d].Name = %q", i, c.Name)
		}
		if c.Args["path"] == nil {
			t.Errorf("calls[%d] lost its path arg — the gate needs it", i)
		}
	}
}

func TestCompactResultElidesBulkKeepsClaims(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := compactResult(map[string]any{
		"content":   long,
		"matches":   []any{1, 2, 3},
		"exit_code": float64(0),
		"sha":       "abc123",
		"created":   true,
		"error":     "boom",
		"nested":    map[string]any{"output": long},
	})
	// Small, claim-bearing fields survive untouched — the ledger reads exactly these.
	for k, want := range map[string]any{"exit_code": float64(0), "sha": "abc123", "created": true, "error": "boom"} {
		if got[k] != want {
			t.Errorf("compactResult dropped or changed %q: got %v, want %v", k, got[k], want)
		}
	}
	// Bulk is GONE — replaced by its size. Not truncated, not sampled: gone. The
	// script saw it; the model does not.
	if got["content"] != nil {
		t.Errorf("content survived into the model's context (%v chars) — that is the leak code mode exists to prevent", got["content"])
	}
	if got["content_chars"] != 5000 {
		t.Errorf("content_chars = %v, want the true length so the model knows what it did not see", got["content_chars"])
	}
	if got["matches"] != nil || got["matches_count"] != 3 {
		t.Errorf("matches should collapse to a count, got matches=%v matches_count=%v", got["matches"], got["matches_count"])
	}
	if nested, _ := got["nested"].(map[string]any); nested["output_chars"] != 5000 {
		t.Errorf("nested bulk not elided: %v", got["nested"])
	}
}

// TestCompactResultElidesAnUnnamedBulkyField: the size fallback. A tool nobody
// anticipated, returning a big string under a field name payloadKeys never heard
// of, must not be able to reintroduce the leak.
func TestCompactResultElidesAnUnnamedBulkyField(t *testing.T) {
	got := compactResult(map[string]any{"transcript": strings.Repeat("y", 1000)})
	if got["transcript"] != nil {
		t.Error("an unnamed bulky field leaked into the model's context")
	}
	if got["transcript_chars"] != 1000 {
		t.Errorf("transcript_chars = %v, want 1000", got["transcript_chars"])
	}
}

// ---------------------------------------------------------------------------
// 3. The jail still holds — a script is not a way out
// ---------------------------------------------------------------------------

func TestJailHoldsInsideAScript(t *testing.T) {
	tools, b := codeModeTools(t, "read_file", "write_file")
	writeUserFile(t, b, "inside.txt", "fine")

	for name, code := range map[string]string{
		"read escapes the jail":  `return read_file({ path: "../../../../etc/passwd" });`,
		"write escapes the jail": `return write_file({ path: "../../../../tmp/pwned.txt", content: "x" });`,
	} {
		t.Run(name, func(t *testing.T) {
			out := runScriptTool(t, tools, code)
			if out.Error == "" {
				t.Fatalf("the script escaped the jail; result = %q", out.Result)
			}
			// It failed the same way a direct call fails, and the attempt is on the record.
			if len(out.Calls) != 1 || out.Calls[0].Result["error"] == nil {
				t.Errorf("the refused call must still be recorded for the gate; calls = %+v", out.Calls)
			}
		})
	}

	// And the control: a path INSIDE the jail works, so the tests above are
	// proving the jail, not a broken binding.
	out := runScriptTool(t, tools, `return read_file({ path: "inside.txt" }).content;`)
	if out.Error != "" || !strings.Contains(out.Result, "fine") {
		t.Fatalf("a legitimate in-jail read failed: err=%q result=%q", out.Error, out.Result)
	}
}

func TestScriptHasNoAmbientCapability(t *testing.T) {
	tools, _ := codeModeTools(t, "read_file")
	// A bare goja VM has none of these. This is the sandbox argument: the script
	// can call the tools we bound and NOTHING else.
	for _, code := range []string{
		`return typeof require;`,
		`return typeof process;`,
		`return typeof fetch;`,
		`return typeof XMLHttpRequest;`,
	} {
		out := runScriptTool(t, tools, code)
		if out.Error != "" {
			t.Fatalf("%s → %s", code, out.Error)
		}
		if out.Result != `"undefined"` {
			t.Errorf("%s = %s, want undefined — the script must have no ambient capability", code, out.Result)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. A script cannot hang or grind a node
// ---------------------------------------------------------------------------

func TestInfiniteLoopIsInterrupted(t *testing.T) {
	tools, _ := codeModeTools(t, "read_file")
	defer swapTimeout(200 * time.Millisecond)()

	done := make(chan runCodeResult, 1)
	go func() { done <- runScriptTool(t, tools, `while (true) {}`) }()

	select {
	case out := <-done:
		if out.Error == "" {
			t.Fatal("an infinite loop returned successfully")
		}
		if !strings.Contains(out.Error, "time limit") {
			t.Errorf("error = %q, want it to name the time limit so the model can fix its script", out.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an infinite loop hung the node — the interrupt did not fire")
	}
}

func TestCallCapStopsARunawayScript(t *testing.T) {
	tools, b := codeModeTools(t, "read_file")
	writeUserFile(t, b, "a.txt", "x")
	defer swapMaxCalls(5)()

	// The script CATCHES its own failures, so a merely-thrown cap would leave it
	// looping forever. The cap must interrupt, not just throw.
	out := runScriptTool(t, tools, `
		let n = 0;
		while (true) {
			try { read_file({ path: "a.txt" }); n++; } catch (e) { /* swallow */ }
		}
	`)
	if out.Error == "" {
		t.Fatal("a runaway script returned successfully")
	}
	if len(out.Calls) > 5 {
		t.Errorf("calls = %d, want the cap (5) to hold even against a script that swallows the error", len(out.Calls))
	}
}

func swapTimeout(d time.Duration) func() {
	prev := runCodeTimeout
	runCodeTimeout = d
	return func() { runCodeTimeout = prev }
}

func swapMaxCalls(n int) func() {
	prev := runCodeMaxCalls
	runCodeMaxCalls = n
	return func() { runCodeMaxCalls = prev }
}

// ---------------------------------------------------------------------------
// 5. Partial failure is the script's to handle
// ---------------------------------------------------------------------------

func TestScriptCatchesAFailedCallAndCarriesOn(t *testing.T) {
	tools, b := codeModeTools(t, "read_file")
	writeUserFile(t, b, "a.txt", "aaa")
	writeUserFile(t, b, "c.txt", "ccc")

	// Three calls; the second one (a file that isn't there) fails. The script
	// handles it itself rather than dying — which is the point of making a failed
	// call a catchable exception.
	out := runScriptTool(t, tools, `
		const out = {};
		for (const f of ["a.txt", "missing.txt", "c.txt"]) {
			try {
				out[f] = read_file({ path: f }).content.trim();
			} catch (e) {
				out[f] = "FAILED: " + e.message;
			}
		}
		return out;
	`)

	if out.Error != "" {
		t.Fatalf("the script died on a failure it caught: %s", out.Error)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out.Result), &got); err != nil {
		t.Fatal(err)
	}
	if got["a.txt"] != "aaa" || got["c.txt"] != "ccc" {
		t.Errorf("the calls either side of the failure should have succeeded; got %v", got)
	}
	if !strings.HasPrefix(got["missing.txt"], "FAILED:") {
		t.Errorf(`missing.txt = %q, want the script's own caught-error string`, got["missing.txt"])
	}
	// All three are on the record, the failure included: the gate sees what was
	// attempted, not just what worked.
	if len(out.Calls) != 3 {
		t.Fatalf("calls = %d, want all 3 recorded", len(out.Calls))
	}
	if out.Calls[1].Result["error"] == nil {
		t.Error("the failed call must be recorded with its error, so a claim over it stays contradictable")
	}
}

// ---------------------------------------------------------------------------
// 6. The model must be able to fix its own script
// ---------------------------------------------------------------------------

func TestScriptErrorsAreActionable(t *testing.T) {
	tools, _ := codeModeTools(t, "read_file")

	cases := map[string]struct{ code, want string }{
		"syntax error": {`const x = ;`, "SyntaxError"},
		"unknown tool": {`return summon_the_kraken({});`, "not defined"},
		"bad args":     {`return read_file("a.txt");`, "object"},
		"uncaught throw": {`
			const a = 1;
			throw new Error("deliberate");
		`, "deliberate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := runScriptTool(t, tools, c.code)
			if out.Error == "" {
				t.Fatalf("expected an error, got result %q", out.Result)
			}
			if !strings.Contains(out.Error, c.want) {
				t.Errorf("error = %q, want it to mention %q", out.Error, c.want)
			}
		})
	}
}

func TestLogsComeBack(t *testing.T) {
	tools, _ := codeModeTools(t, "read_file")
	out := runScriptTool(t, tools, `
		console.log("hello", 42);
		console.log({ a: 1 });
		return "done";
	`)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	if len(out.Logs) != 2 || out.Logs[0] != "hello 42" || out.Logs[1] != `{"a":1}` {
		t.Errorf("logs = %q, want the script's printed output (objects as JSON)", out.Logs)
	}
}

// ---------------------------------------------------------------------------
// 7. Which tools code mode may bind
// ---------------------------------------------------------------------------

// TestConfirmTierToolIsNotInTheScriptAPI: a script has nowhere to suspend to, so
// a tool that pauses the node for a human (confirm tier — guard.go) must not
// become a function inside one. Mid-script, that pause has no turn boundary to
// land on, and resuming would re-run the script from the top, re-doing every side
// effect it had already performed. It stays available as an ordinary
// one-call-per-turn tool: code mode adds a path, it removes none.
func TestConfirmTierToolIsNotInTheScriptAPI(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build([]string{"read_file", "delete_path", vetting.RunCodeToolName}, Deps{
		Workspace:       j,
		WorkspaceUserID: "u1",
		Guards:          map[string]string{"delete_path": "confirm"},
		SafetyJudge: func(context.Context, string, string, string, map[string]any, string) (bool, string, error) {
			return true, "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	names := map[string]bool{}
	for _, b := range built {
		names[b.Name()] = true
		if b.Name() == vetting.RunCodeToolName {
			desc = b.Description()
		}
	}
	if !names["delete_path"] {
		t.Error("delete_path was removed from the agent's tools; code mode must not remove anything")
	}
	if strings.Contains(desc, "delete_path(") {
		t.Error("a confirm-tier tool is in the script API: a mid-script human pause has nowhere to land, and resuming would re-run the script's side effects")
	}
	if !strings.Contains(desc, "read_file(") {
		t.Fatal("the unguarded tool should still be in the API")
	}
}

// longRunningTool ends the model's turn by design and is answered on the next one
// (ask_user, get_user_choice) — so, like a confirm-tier tool, it cannot live
// inside a script.
func TestLongRunningToolIsNotInTheScriptAPI(t *testing.T) {
	slow, err := functiontool.New[fakeToolArgs, fakeToolResult](
		functiontool.Config{Name: "await_the_oracle", Description: "Waits.", IsLongRunning: true},
		func(_ adkagent.Context, a fakeToolArgs) (fakeToolResult, error) { return fakeToolResult{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	quick, err := functiontool.New[fakeToolArgs, fakeToolResult](
		functiontool.Config{Name: "divine_the_corpus", Description: "Divines."},
		func(_ adkagent.Context, a fakeToolArgs) (fakeToolResult, error) { return fakeToolResult{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := newRunCode([]tool.Tool{slow, quick}, func(tl tool.Tool) bool { return noCodeMode(tl, Deps{}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rc.Description(), "await_the_oracle(") {
		t.Error("a long-running tool is in the script API: it ends the model's turn by design")
	}
	if !strings.Contains(rc.Description(), "divine_the_corpus(") {
		t.Fatal("the ordinary tool should still be in the API")
	}
}

func TestRunCodeNeedsSomethingToBind(t *testing.T) {
	if _, err := newRunCode(nil, nil); err == nil {
		t.Fatal("run_code with no tools to expose should be an error, not an empty API")
	}
}
