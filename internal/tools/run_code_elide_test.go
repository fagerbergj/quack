package tools

import (
	"strings"
	"testing"
)

// Code mode ENFORCES its contract: a script may read whatever it likes, but it does
// not get to hand the bulk back to itself.
//
// THE LIVE FAILURE (2026-07-14, code mode's first live run). run_code's description
// already said, in capitals, "RETURN ONLY WHAT YOU NEED — NEVER THE FILE CONTENTS",
// with a worked example. The model's first script did this anyway:
//
//	out[f] = { total_lines: content.total_lines, content: content.content };
//
// and returned 52.2 KB of file contents. The `calls` ledger elided the payloads
// correctly; the model simply walked around the wall by another route. Asking did not
// work — so we no longer ask.
//
// No other harness enforces this. Cloudflare's Code Mode "relies entirely on the
// language model's own judgment" (their own docs); goose returns whatever its runtime
// printed. They run 200k-context frontier models, where a dump is waste. On a 65k
// window it is the failure the feature exists to prevent.
func TestEchoedFileContentIsElidedNotDelivered(t *testing.T) {
	fileContent := strings.Repeat("func Handler(w http.ResponseWriter, r *http.Request) {}\n", 300) // ~16 KB
	r := &scriptRun{}
	r.recordPayload(map[string]any{"content": fileContent})

	// The model's mistake, verbatim: return {path, content}.
	returned := map[string]any{
		"registry.go": map[string]any{"total_lines": float64(300), "content": fileContent},
	}

	out, cut := r.elideEchoes(returned)
	if cut < len(fileContent) {
		t.Fatalf("elided %d bytes, want >= %d — the file contents were DELIVERED to the model", cut, len(fileContent))
	}
	got := out.(map[string]any)["registry.go"].(map[string]any)
	if s, _ := got["content"].(string); strings.Contains(s, "func Handler") {
		t.Fatal("the file's text is still in the return value — code mode delivered exactly what it exists to withhold")
	}
	if s, _ := got["content"].(string); !strings.Contains(s, "elided by code mode") {
		t.Errorf("the elision must SAY what happened, or the model cannot learn from it; got %q", s)
	}
	// The structure the model legitimately wanted is untouched.
	if got["total_lines"] != float64(300) {
		t.Errorf("total_lines = %v — eliding the content must not damage the structure around it", got["total_lines"])
	}
	if w := r.echoWarning(cut); !strings.Contains(w, "DROPPED") || !strings.Contains(w, "KB") {
		t.Errorf("the warning must name what was dropped and how much; got %q", w)
	}
}

// A COMPUTED answer is never touched — this is the case that makes eliding safe.
// A patch, a diff, a generated file: none of them is a verbatim substring of what any
// tool returned (their own markers and interleaving break containment), so the
// detector cannot mistake one for an echo. If this test ever fails, eliding has
// started destroying real work and must be reverted.
func TestComputedAnswerIsNeverElided(t *testing.T) {
	fileContent := strings.Repeat("old line\n", 500)
	r := &scriptRun{}
	r.recordPayload(map[string]any{"content": fileContent})

	// A unified diff built FROM those lines: contains them, but not verbatim.
	var patch strings.Builder
	patch.WriteString("--- a/x.go\n+++ b/x.go\n@@ -1,500 +1,500 @@\n")
	for i := 0; i < 500; i++ {
		patch.WriteString("-old line\n+new line\n")
	}

	out, cut := r.elideEchoes(map[string]any{"patch": patch.String()})
	if cut != 0 {
		t.Fatalf("elided %d bytes of a COMPUTED patch — code mode just destroyed the script's actual work", cut)
	}
	if got := out.(map[string]any)["patch"].(string); !strings.Contains(got, "+new line") {
		t.Fatal("the computed patch did not survive")
	}
}

// A short verbatim quote is a legitimate answer — a signature, a failing assertion,
// the three lines around a bug — and must pass through untouched.
func TestShortQuoteSurvives(t *testing.T) {
	fileContent := strings.Repeat("x", 50_000) + "\nfunc newGuardedTool(t tool.Tool, tier guardTier) (tool.Tool, error) {\n"
	r := &scriptRun{}
	r.recordPayload(map[string]any{"content": fileContent})

	quote := "func newGuardedTool(t tool.Tool, tier guardTier) (tool.Tool, error) {"
	out, cut := r.elideEchoes(map[string]any{"signature": quote})
	if cut != 0 {
		t.Fatalf("elided %d bytes of a short quote — a quoted signature is the answer, not a dump", cut)
	}
	if out.(map[string]any)["signature"] != quote {
		t.Fatal("the quote was altered")
	}
}

// Nothing echoed ⇒ nothing elided, nothing warned, and the value is returned as-is
// (the common case must not be rebuilt or reordered).
func TestCleanReturnIsUntouched(t *testing.T) {
	r := &scriptRun{}
	r.recordPayload(map[string]any{"content": strings.Repeat("y", 20_000)})

	summary := map[string]any{"path": "registry.go", "total_lines": float64(273), "exports": []any{"Build", "Deps"}}
	out, cut := r.elideEchoes(summary)
	if cut != 0 {
		t.Fatalf("elided %d bytes of a clean summary", cut)
	}
	if r.echoWarning(cut) != "" {
		t.Error("a clean summary must not be warned at")
	}
	if got := out.(map[string]any); got["path"] != "registry.go" || len(got["exports"].([]any)) != 2 {
		t.Fatalf("the summary was damaged: %v", got)
	}
}
