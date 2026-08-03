package acp

import (
	"context"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestBundlePromptsCallableUnderOpencodeNamespacing pins #630's fix: opencode
// namespaces an MCP server's tools as "<serverName>_<toolName>" on its OWN
// side before ever handing them to the model - quack's server (memoryMCPHandler)
// registers bare names and knows nothing about that prefix. Seen live in a
// recorded ACP session ("quack-memory_load_memory") and documented at
// https://opencode.ai/docs/mcp-servers/ ("MCP server tools are registered with
// server name as prefix"). A bundle prompt that only ever names the bare form
// leaves an agent unable to find its tools by that name, exactly as #630
// diagnosed - the fix is telling the agent to expect the prefixed form.
//
// This derives the server name and the real registered tool names from the
// SAME live server production code builds, never hardcoded, so a future
// rename (#628) can't silently desync this test from reality: it proves each
// documented tool (a) is actually registered and working, and (b) the
// bundle's prompt explicitly warns that opencode may hand it back prefixed.
func TestBundlePromptsCallableUnderOpencodeNamespacing(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	pr := &vetting.PRStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: review, PRStage: pr})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	// The server's own advertised name is what opencode would prefix onto
	// every tool it exposes for this session (memoryMCPServers hands opencode
	// the SAME name via session/new's mcpServers - see memorymcp.go).
	serverName := cs.InitializeResult().ServerInfo.Name
	if serverName == "" {
		t.Fatal("server advertised no name in the initialize handshake")
	}

	toolsRes, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range toolsRes.Tools {
		registered[tool.Name] = true
	}

	prefixNote := regexp.MustCompile("(?i)<server>_<name>")

	// Every bundle whose prompt.md instructs the agent to call one of these
	// bare-named tools must ALSO warn it that opencode may expose the tool
	// prefixed - so a model reasoning from the prompt alone still finds it.
	for _, bundle := range []string{"agents/code-reviewer", "agents/code-implementer"} {
		b, err := agent.LoadBundle(bundle)
		if err != nil {
			t.Fatalf("LoadBundle(%q): %v", bundle, err)
		}
		if !prefixNote.MatchString(b.Prompt) {
			t.Errorf("%s/prompt.md: missing the note that a tool can appear as <server>_<name> (opencode's real exposure format) - see #630", bundle)
		}
	}

	// Every tool the prompts document by bare name must really be registered -
	// this is what would catch reviewmcp.go/memorymcp.go renaming a tool
	// without the prompt text following.
	documented := []string{"stage_review_comment", "stage_review", "stage_pr"}
	for _, name := range documented {
		if !registered[name] {
			t.Fatalf("bundle prompts document tool %q but the server never registers it; check internal/acp/reviewmcp.go", name)
		}
	}

	// Prove the tools genuinely work end to end (the bare name is what THIS
	// server understands; opencode's client-side rename is a layer quack's
	// server never sees, so it can't be exercised without a real opencode
	// round-trip - see the PR body for a live one run against this binary).
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s returned a tool error: %s", name, toolResultText(t, res))
		}
		return res
	}

	call("stage_review_comment", map[string]any{"path": "internal/acp/toolnaming_test.go", "line": 1, "body": "nit: staged via bare name"})
	call("stage_review", map[string]any{"event": "approve", "body": "staged via bare name"})
	sd, ok := review.Snapshot()
	if !ok || sd.Event != "approve" || len(sd.Comments) != 1 {
		t.Fatalf("stage_review/stage_review_comment calls didn't land in the review buffer: ok=%v %+v", ok, sd)
	}

	call("stage_pr", map[string]any{"title": "fix: staged via bare name", "body": "landed"})
	prd, ok := pr.Snapshot()
	if !ok || prd.Title != "fix: staged via bare name" {
		t.Fatalf("stage_pr call didn't land in the PR buffer: ok=%v %+v", ok, prd)
	}
}
