package vetting

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
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

// TestLoadArtifact_ScopedToOwnChat proves LoadArtifact reads exactly the
// MemSession's own (AppName, UserID, ChatID) - a node can never reach another
// chat's artifact by naming it, since scope comes from registration, not args.
func TestLoadArtifact_ScopedToOwnChat(t *testing.T) {
	svc := artifact.InMemoryService()
	SetArtifactService(svc)
	t.Cleanup(func() { SetArtifactService(nil) })

	mustSaveArtifact(t, svc, "quack", "u1", "chat-a", "comments", "chat A's comments")
	mustSaveArtifact(t, svc, "quack", "u1", "chat-b", "comments", "chat B's comments")

	sessA := MemSession{AppName: "quack", UserID: "u1", ChatID: "chat-a"}
	resp, err := LoadArtifact(context.Background(), sessA, "comments")
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	got := string(resp.Part.InlineData.Data)
	if got != "chat A's comments" {
		t.Fatalf("got %q, want chat A's own artifact", got)
	}
	if strings.Contains(got, "chat B") {
		t.Fatalf("leaked chat B's artifact: %q", got)
	}
}

// TestLoadArtifact_NoServiceConfigured proves an unwired backend fails the
// call cleanly rather than panicking - the degrade-gracefully case.
func TestLoadArtifact_NoServiceConfigured(t *testing.T) {
	SetArtifactService(nil)
	if ArtifactsEnabled() {
		t.Fatal("ArtifactsEnabled() true with no service set")
	}
	if _, err := LoadArtifact(context.Background(), MemSession{AppName: "quack", ChatID: "c"}, "comments"); err == nil {
		t.Fatal("want an error with no artifact service configured")
	}
}

// TestLoadArtifact_NoScope proves a MemSession that predates artifact scoping
// (empty AppName/UserID/ChatID) can't read anything either.
func TestLoadArtifact_NoScope(t *testing.T) {
	SetArtifactService(artifact.InMemoryService())
	t.Cleanup(func() { SetArtifactService(nil) })
	if _, err := LoadArtifact(context.Background(), MemSession{}, "comments"); err == nil {
		t.Fatal("want an error for a scopeless MemSession")
	}
}
