package acp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestReadArtifactMCP_ReadsOwnChatArtifact proves an ACP agent can read an
// artifact saved to its own chat through the loopback MCP surface.
func TestReadArtifactMCP_ReadsOwnChatArtifact(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: "quack", UserID: "u1", SessionID: "chat-a", FileName: "notes.txt",
		Part: genai.NewPartFromBytes([]byte("hello from chat a"), "text/plain"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "notes.txt"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_artifact returned an error: %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "hello from chat a") {
		t.Fatalf("read_artifact result = %q, want content of notes.txt", text)
	}
}

// TestReadArtifactMCP_CrossSessionDenied is the security property: a node
// registered for chat-a can never read chat-b's artifact, because the tool's
// scope (app/user/chat) comes only from the registered session, never from a
// tool argument - there is no session id in read_artifact's input at all.
func TestReadArtifactMCP_CrossSessionDenied(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: "quack", UserID: "u1", SessionID: "chat-b", FileName: "secret.txt",
		Part: genai.NewPartFromBytes([]byte("chat b's secret"), "text/plain"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	// The tool only accepts a filename - there is no way to pass chat-b's id.
	// Asking for chat-b's file by name still resolves against chat-a's scope
	// and must fail, since chat-a never had "secret.txt" saved to it.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "secret.txt"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	if !res.IsError {
		t.Fatalf("read_artifact must fail for another chat's artifact, got: %s", toolResultText(t, res))
	}
}

// TestReadArtifactMCP_AbsentServiceMeansToolUnavailable: a node without an
// artifact service degrades gracefully - the tool is simply not registered,
// no panic.
func TestReadArtifactMCP_AbsentServiceMeansToolUnavailable(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{}) // no Artifacts
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "x"}}); err == nil {
		t.Fatal("read_artifact must be unavailable when no artifact service is wired, but the call succeeded")
	}
}
