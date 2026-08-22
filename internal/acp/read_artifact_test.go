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

func mustSaveArtifact(t *testing.T, svc artifact.Service, appName, userID, chatID, name, text string) {
	t.Helper()
	_, err := svc.Save(context.Background(), &artifact.SaveRequest{
		AppName: appName, UserID: userID, SessionID: chatID, FileName: name,
		Part: &genai.Part{InlineData: &genai.Blob{Data: []byte(text), MIMEType: "text/plain"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestReadArtifactMCP_ReadsOwnChat proves the ACP loopback surface's
// read_artifact tool reads the artifact scoped to the registered session.
func TestReadArtifactMCP_ReadsOwnChat(t *testing.T) {
	svc := artifact.InMemoryService()
	vetting.SetArtifactService(svc)
	t.Cleanup(func() { vetting.SetArtifactService(nil) })
	mustSaveArtifact(t, svc, "quack", "u1", "chat-a", "comments", "hello from chat A")

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "comments"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	if got := toolResultText(t, res); got != "hello from chat A" {
		t.Fatalf("got %q, want the chat's own artifact", got)
	}
}

// TestReadArtifactMCP_CrossChatIsolation proves a node scoped to chat A can't
// read chat B's artifact by name - the security property the last brief
// called out explicitly.
func TestReadArtifactMCP_CrossChatIsolation(t *testing.T) {
	svc := artifact.InMemoryService()
	vetting.SetArtifactService(svc)
	t.Cleanup(func() { vetting.SetArtifactService(nil) })
	mustSaveArtifact(t, svc, "quack", "u1", "chat-a", "comments", "chat A's secret")
	mustSaveArtifact(t, svc, "quack", "u1", "chat-b", "comments", "chat B's secret")

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "comments"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	got := toolResultText(t, res)
	if strings.Contains(got, "chat B") {
		t.Fatalf("chat A's session read chat B's artifact: %q", got)
	}
	if got != "chat A's secret" {
		t.Fatalf("got %q, want only chat A's own artifact", got)
	}
}

// TestReadArtifactMCP_UnscopedSessionGetsNoTool proves a node with no
// artifact scope (the common case before this change) doesn't even get the
// tool offered - nothing reachable today became newly-reachable by accident.
func TestReadArtifactMCP_UnscopedSessionGetsNoTool(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "comments"}}); err == nil {
		t.Fatal("want an error calling read_artifact on an unscoped session - tool should not be registered")
	}
}
