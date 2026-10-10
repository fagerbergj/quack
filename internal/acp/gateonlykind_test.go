package acp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestGateOnlyKindsNeverOfferedAsWorkerTools: judge_round and delivery_record (gate-written) never appear
// as write_<kind> in mcpToolNames or on the live MCP server, and a direct call is an unknown tool.
func TestGateOnlyKindsNeverOfferedAsWorkerTools(t *testing.T) {
	secret := mustMemSecret(t)
	sess := vetting.MemSession{
		Artifacts: artifact.InMemoryService(),
		AppName:   "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Review: &vetting.ReviewStage{}, // code-reviewer round shape
	}
	vetting.RegisterMemSession(secret, sess)
	defer vetting.UnregisterMemSession(secret)

	gateOnly := []string{writeKindPrefix + "judge_round", writeKindPrefix + "delivery_record"}

	names := mcpToolNames(sess, true)
	for _, n := range names {
		for _, g := range gateOnly {
			if n == mcpServerName+"_"+g {
				t.Errorf("mcpToolNames offered %q, a gate-only kind", n)
			}
		}
	}

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	toolsRes, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	registered := map[string]bool{}
	for _, tl := range toolsRes.Tools {
		registered[tl.Name] = true
	}
	for _, g := range gateOnly {
		if registered[g] {
			t.Errorf("live MCP server registered %q, a gate-only kind", g)
		}
	}

	// A direct call for a gate-only kind's write tool must be rejected -
	// there's no tool by that name for the server to run.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      writeKindPrefix + "judge_round",
		Arguments: map[string]any{"passed": true, "score": 3},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("a direct write_judge_round call must be rejected (unknown tool), not accepted")
	}

	// Sanity: the underlying kind really is registered and Structured -
	// this test is about the tool surface, not the kind's existence.
	spec, ok := recordstore.SpecFor("judge_round")
	if !ok || spec.Class != recordstore.Structured || spec.AgentWritable {
		t.Fatalf("judge_round spec = %+v (ok=%v), want Structured and AgentWritable=false", spec, ok)
	}
}
