package tools

import (
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/vetting"
)

type checkMermaidToolCtx struct{ *fakeCtx }

func (checkMermaidToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// requireNode provisions the SAME scripts/node_modules as vetting's
// requireMermaidValidator (via vetting.EnsureMermaidValidatorDeps), so the two
// packages' test binaries - run in parallel by `go test ./...` - don't race
// each other with independent `npm ci` runs in the same directory.
func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("SKIPPING check_mermaid test: node not on PATH")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("SKIPPING check_mermaid test: npm not on PATH")
	}
	if err := vetting.EnsureMermaidValidatorDeps(); err != nil {
		t.Skipf("SKIPPING check_mermaid test: could not provision scripts/node_modules: %v", err)
	}
}

func runCheckMermaid(t *testing.T, diagram string) string {
	t.Helper()
	tl, err := newCheckMermaid(Deps{})
	if err != nil {
		t.Fatalf("newCheckMermaid: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("check_mermaid tool is not runnable")
	}
	res, err := rt.Run(checkMermaidToolCtx{newFakeCtx()}, map[string]any{"diagram": diagram})
	if err != nil {
		t.Fatalf("check_mermaid Run: %v", err)
	}
	out, _ := res["result"].(string)
	return out
}

func TestCheckMermaidTool_ValidDiagram(t *testing.T) {
	// Parallel: each run spawns node loading mermaid's full parser (~1.5s);
	// nothing in this test binary reassigns the validator's globals, so
	// the two runs can overlap.
	t.Parallel()
	requireNode(t)
	got := runCheckMermaid(t, "flowchart TD\n    A[Start] --> B[Finish]")
	if got != "ok" {
		t.Fatalf("result = %q, want %q", got, "ok")
	}
}

func TestCheckMermaidTool_InvalidDiagram(t *testing.T) {
	t.Parallel()
	requireNode(t)
	got := runCheckMermaid(t, "A[Start] --> B[Finish]") // no diagram-type declaration
	if !strings.HasPrefix(got, "invalid") {
		t.Fatalf("result = %q, want it to start with %q", got, "invalid")
	}
}
