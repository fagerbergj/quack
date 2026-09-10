package rest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestCloseNodeSessions_RemovesACPState pins the cleanup move: chat
// archive/delete (DeleteChat/UpdateChat's *body.Archived branch) must
// remove a chat's ACP session state, which lives outside the workspace tree
// RemoveChatScope reaches (one dir per node under HomeDir - see
// workspace.Jail.ACPStateDir/RemoveChatACPState).
func TestCloseNodeSessions_RemovesACPState(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	stateDir, err := jail.ACPStateDir(userID, "chat1", "n1")
	if err != nil {
		t.Fatalf("ACPStateDir: %v", err)
	}

	closeNodeSessions(jail, "chat1")

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("acp state dir %q still exists after closeNodeSessions: %v", stateDir, err)
	}
}

// TestCloseNodeSessions_NilJailNoOp: a server run without workspace
// configured (nil jail, the default in newTestHandler) must not panic.
func TestCloseNodeSessions_NilJailNoOp(t *testing.T) {
	closeNodeSessions(nil, "chat1") // must not panic
}

// TestDeleteChat_ClosesNodeSessions is DeleteChat's REST-level assertion:
// deleting a chat removes its ACP state alongside its workspace tree.
func TestDeleteChat_ClosesNodeSessions(t *testing.T) {
	h := newTestHandler(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	h.jail = jail
	chatID := mustCreateChat(t, h)
	stateDir, err := jail.ACPStateDir(userID, chatID, "n1")
	if err != nil {
		t.Fatalf("ACPStateDir: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chats/"+chatID, nil)
	rec := httptest.NewRecorder()
	h.DeleteChat(rec, req, chatID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("acp state dir %q still exists after DeleteChat: %v", stateDir, err)
	}
}

// TestUpdateChat_ArchiveClosesNodeSessions: archiving a chat (not deleting
// it) is the OTHER trigger the design names - a terminal node's session must
// stop being resumable once the chat is archived, same as delete.
func TestUpdateChat_ArchiveClosesNodeSessions(t *testing.T) {
	h := newTestHandler(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	h.jail = jail
	chatID := mustCreateChat(t, h)
	stateDir, err := jail.ACPStateDir(userID, chatID, "n1")
	if err != nil {
		t.Fatalf("ACPStateDir: %v", err)
	}

	trueVal := true
	rec := patchUpdateChat(t, h, chatID, schema.UpdateChatBody{Archived: &trueVal})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("acp state dir %q still exists after archiving the chat: %v", stateDir, err)
	}
}
