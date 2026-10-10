package acp

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/vetting"
)

// forbiddenToolCheckWording: no ACP bundle prompt may tell an agent to check whether its tools exist; a bash
// probe can't see MCP tools, and the preamble states the offered names as fact.
var forbiddenToolCheckWording = regexp.MustCompile(`(?i)check your (actual )?tool list|not in your tool list`)

// TestBundlePromptsDoNotAskAgentToCheckToolExistence: an ACP subprocess can't prove an MCP tool absent, so no
// bundle prompt may invite self-verification; the preamble states the names (mcpToolNames/mcpToolsBlock).
func TestBundlePromptsDoNotAskAgentToCheckToolExistence(t *testing.T) {
	for _, bundle := range []string{
		"../../.agents/plugins/github/agents/code-reviewer",
		"../../.agents/plugins/github/agents/code-implementer",
		"../../.agents/plugins/github/agents/code-explorer",
	} {
		b, err := agent.LoadBundle(context.Background(), nil, bundle)
		if err != nil {
			t.Fatalf("LoadBundle(%q): %v", bundle, err)
		}
		if forbiddenToolCheckWording.MatchString(b.Prompt) {
			t.Errorf("%s/prompt.md: still tells the agent to check its own tool list - the generated preamble should state the fact instead (#688)", bundle)
		}
	}
}

// TestMCPToolNamesMatchTheLiveServer: mcpToolNames names exactly what memoryMCPHandler registers for the
// same session, so a tool rename can't desync the preamble.
func TestMCPToolNamesMatchTheLiveServer(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	review := &vetting.ReviewStage{}
	pr := &vetting.PRStage{}
	sess := vetting.MemSession{Review: review, PRStage: pr}
	vetting.RegisterMemSession(secret, sess)
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	// The server's own advertised name is what the pi-acp shim prefixes onto every
	// tool it exposes (memoryMCPServers hands it the SAME name via session/new's mcpServers - see memorymcp.go).
	serverName := cs.InitializeResult().ServerInfo.Name
	if serverName != mcpServerName {
		t.Fatalf("server advertised name %q, want the mcpServerName const %q", serverName, mcpServerName)
	}

	toolsRes, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range toolsRes.Tools {
		registered[tool.Name] = true
	}

	names := mcpToolNames(sess, true)
	if len(names) == 0 {
		t.Fatal("mcpToolNames returned no tools for a session with Review and PRStage set")
	}
	for _, name := range names {
		bare := strings.TrimPrefix(name, mcpServerName+"_")
		if bare == name {
			t.Errorf("generated name %q does not carry the %q prefix", name, mcpServerName)
		}
		if !registered[bare] {
			t.Errorf("mcpToolNames names %q (bare %q) but the server never registers it; check internal/acp/reviewmcp.go", name, bare)
		}
	}

	// mcpToolNames returns nil and the block says "none" when the surface wasn't offered: loud, not omitted.
	if got := mcpToolNames(sess, false); got != nil {
		t.Errorf("mcpToolNames(offered=false) = %v, want nil", got)
	}
	if got := mcpToolsBlock(nil); !strings.Contains(got, "none") {
		t.Errorf("mcpToolsBlock(nil) = %q, want it to say none", got)
	}

	// Every tool the bundle prompts document by bare name must be registered, catching a rename
	// the prompt text didn't follow.
	documented := []string{"stage_review_comment", "stage_review", "stage_pr"}
	for _, name := range documented {
		if !registered[name] {
			t.Fatalf("bundle prompts document tool %q but the server never registers it; check internal/acp/reviewmcp.go", name)
		}
	}

	// Prove the tools genuinely work end to end - the bare name is what THIS
	// server understands; the shim's client-side rename is a layer quack's server never sees, so it can't be exercised here.
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
	call("stage_review", map[string]any{"event": "approve", "takeaway": "staged via bare name"})
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

// hasToolName reports whether names (mcpToolNames' output - server-prefixed)
// includes the given bare tool name.
func hasToolName(names []string, bare string) bool {
	for _, n := range names {
		if n == mcpServerName+"_"+bare {
			return true
		}
	}
	return false
}

// TestMCPToolNames_SelectsPushForExistingPR: mcpToolNames names exactly one of stage_pr/stage_push,
// keyed on MemSession.ExistingPR.
func TestMCPToolNames_SelectsPushForExistingPR(t *testing.T) {
	newPR := vetting.MemSession{PRStage: &vetting.PRStage{}}
	names := mcpToolNames(newPR, true)
	if !hasToolName(names, "stage_pr") || hasToolName(names, "stage_push") {
		t.Errorf("new-PR session: names = %v, want stage_pr only", names)
	}

	existingPR := vetting.MemSession{PRStage: &vetting.PRStage{}, ExistingPR: true}
	names = mcpToolNames(existingPR, true)
	if !hasToolName(names, "stage_push") || hasToolName(names, "stage_pr") {
		t.Errorf("existing-PR session: names = %v, want stage_push only", names)
	}
}
