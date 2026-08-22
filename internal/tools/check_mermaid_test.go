package tools

import (
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/toolconfirmation"
)

type checkMermaidToolCtx struct{ *fakeCtx }

func (checkMermaidToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// requireNode skips when the mermaid validator's runtime isn't available -
// mirrors vetting's requireMermaidValidator posture for this package.
func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("SKIPPING check_mermaid test: node not on PATH")
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
	requireNode(t)
	got := runCheckMermaid(t, "flowchart TD\n    A[Start] --> B[Finish]")
	if got != "ok" {
		t.Fatalf("result = %q, want %q", got, "ok")
	}
}

func TestCheckMermaidTool_InvalidDiagram(t *testing.T) {
	requireNode(t)
	got := runCheckMermaid(t, "A[Start] --> B[Finish]") // no diagram-type declaration
	if !strings.HasPrefix(got, "invalid") {
		t.Fatalf("result = %q, want it to start with %q", got, "invalid")
	}
}
