package acp

import (
	"bytes"
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

// TestReadArtifactMCP_ScopedToRegisteredSession pins scope selection itself:
// the same filename exists in both chats with different content, so a broken
// scope that resolves to *something* (not just "nothing") would still be caught.
func TestReadArtifactMCP_ScopedToRegisteredSession(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	for chat, content := range map[string]string{
		"chat-a": "chat a's shared.txt",
		"chat-b": "chat b's shared.txt",
	} {
		if _, err := svc.Save(ctx, &artifact.SaveRequest{
			AppName: "quack", UserID: "u1", SessionID: chat, FileName: "shared.txt",
			Part: genai.NewPartFromBytes([]byte(content), "text/plain"),
		}); err != nil {
			t.Fatalf("Save(%s): %v", chat, err)
		}
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "shared.txt"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_artifact returned an error: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "chat a's shared.txt") {
		t.Fatalf("read_artifact result = %q, want chat-a's content", text)
	}
	if strings.Contains(text, "chat b's shared.txt") {
		t.Fatalf("read_artifact result = %q, leaked chat-b's content", text)
	}
}

// TestReadArtifactMCP_OversizedContentIsCapped: an artifact past the size
// limit is refused with a clear marker instead of dumping raw bytes into context.
func TestReadArtifactMCP_OversizedContentIsCapped(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	big := bytes.Repeat([]byte("x"), readArtifactMaxBytes+1)
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: "quack", UserID: "u1", SessionID: "chat-a", FileName: "big.bin",
		Part: genai.NewPartFromBytes(big, "application/octet-stream"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "big.bin"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	text := toolResultText(t, res)
	if strings.Contains(text, strings.Repeat("x", 100)) {
		t.Fatalf("read_artifact returned raw oversized content instead of a refusal")
	}
	if !strings.Contains(text, "too large") {
		t.Fatalf("read_artifact result = %q, want a size-limit refusal marker", text)
	}
}

// TestReadArtifactMCP_AtSizeLimitIsReturned: the boundary itself (exactly at
// the cap) must still succeed - only content strictly over the cap is refused.
func TestReadArtifactMCP_AtSizeLimitIsReturned(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	atLimit := bytes.Repeat([]byte("y"), readArtifactMaxBytes)
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: "quack", UserID: "u1", SessionID: "chat-a", FileName: "atlimit.txt",
		Part: genai.NewPartFromBytes(atLimit, "text/plain"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a"})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_artifact", Arguments: map[string]any{"name": "atlimit.txt"}})
	if err != nil {
		t.Fatalf("CallTool read_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_artifact at exactly the size limit must succeed, got: %s", toolResultText(t, res))
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
