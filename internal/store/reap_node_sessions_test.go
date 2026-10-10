package store

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/session"
)

// DeleteChat reaps each DAG node's worker session (AppName = the node's bundle, id "<chatID>:<nodeID>")
// and its retry session by chat id alone, without knowing which bundle ran which node.
func TestDeleteChat_ReapsPerNodeWorkerSessions(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	c, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	realChatID := c.ID

	type addr struct{ app, user, id string }
	rows := []addr{
		{"code-implementer", "A2A_USER_" + realChatID + ":n1", realChatID + ":n1"},
		{"code-reviewer", "A2A_USER_" + realChatID + ":n2", realChatID + ":n2"},
		{"quack", "local", realChatID + "::retry"},
	}
	for _, r := range rows {
		resp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: r.app, UserID: r.user, SessionID: r.id})
		if err != nil {
			t.Fatalf("session Create %+v: %v", r, err)
		}
		if err := st.Sessions.AppendEvent(ctx, resp.Session, session.NewEvent(ctx, "test")); err != nil {
			t.Fatalf("AppendEvent %+v: %v", r, err)
		}
	}

	// A different chat's node session must NOT be swept by this chat's delete.
	other, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: "code-implementer", UserID: "A2A_USER_other-chat:n1", SessionID: "other-chat:n1"})
	if err != nil {
		t.Fatalf("session Create other chat: %v", err)
	}

	if err := st.DeleteChat(ctx, realChatID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	for _, r := range rows {
		if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: r.app, UserID: r.user, SessionID: r.id}); err == nil && resp != nil && resp.Session != nil {
			t.Errorf("session %+v still present after DeleteChat, want reaped", r)
		}
	}
	var eventCount int64
	if err := st.db.Table("events").Where("session_id LIKE ?", realChatID+"%").Count(&eventCount).Error; err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("events rows for chat %q after DeleteChat = %d, want 0", realChatID, eventCount)
	}

	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: "code-implementer", UserID: "A2A_USER_other-chat:n1", SessionID: "other-chat:n1"}); err != nil || resp == nil || resp.Session == nil {
		t.Errorf("unrelated chat's session was reaped by this chat's DeleteChat: %v (session=%v)", err, other)
	}
}

// Archiving reaps per-node worker sessions like DeleteChat; un-archiving doesn't, and a re-run is a no-op,
// not an error.
func TestArchiveChat_ReapsPerNodeWorkerSessionsOnlyWhenArchiving(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	c, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	chatID := c.ID

	resp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: "code-implementer", UserID: "A2A_USER_" + chatID + ":n1", SessionID: chatID + ":n1"})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	if err := st.Sessions.AppendEvent(ctx, resp.Session, session.NewEvent(ctx, "test")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if err := st.ArchiveChat(ctx, chatID, false); err != nil {
		t.Fatalf("ArchiveChat(false): %v", err)
	}
	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: "code-implementer", UserID: "A2A_USER_" + chatID + ":n1", SessionID: chatID + ":n1"}); err != nil || resp == nil || resp.Session == nil {
		t.Fatalf("node session reaped by ArchiveChat(false), want it left alone: %v", err)
	}

	if err := st.ArchiveChat(ctx, chatID, true); err != nil {
		t.Fatalf("ArchiveChat(true): %v", err)
	}
	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: "code-implementer", UserID: "A2A_USER_" + chatID + ":n1", SessionID: chatID + ":n1"}); err == nil && resp != nil && resp.Session != nil {
		t.Error("node session still present after ArchiveChat(true), want reaped")
	}

	// The chat itself must still be archived and readable - the reap is a
	// side effect, never a reason to fail the archive.
	got, err := st.GetChat(ctx, chatID)
	if err != nil || got == nil || !got.Archived {
		t.Fatalf("GetChat after ArchiveChat(true) = %+v, err=%v, want archived=true", got, err)
	}
}

// A chat id with a literal "_" (e.g. "ext:github:owner/my_repo#42") must not LIKE-match a different chat's
// live worker session; likeEscape escapes the wildcard.
func TestReapNodeSessions_UnderscoreDoesNotWidenMatch(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	const target = "chat_1"   // literal underscore
	const collider = "chatX1" // "_" in target's LIKE pattern would also match this
	for _, id := range []string{target, collider} {
		resp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: "code-implementer", UserID: "A2A_USER_" + id + ":n1", SessionID: id + ":n1"})
		if err != nil {
			t.Fatalf("session Create %q: %v", id, err)
		}
		if err := st.Sessions.AppendEvent(ctx, resp.Session, session.NewEvent(ctx, "test")); err != nil {
			t.Fatalf("AppendEvent %q: %v", id, err)
		}
	}

	if err := st.ReapNodeSessions(ctx, target); err != nil {
		t.Fatalf("ReapNodeSessions: %v", err)
	}

	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: "code-implementer", UserID: "A2A_USER_" + target + ":n1", SessionID: target + ":n1"}); err == nil && resp != nil && resp.Session != nil {
		t.Errorf("target chat %q session still present after its own ReapNodeSessions", target)
	}
	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: "code-implementer", UserID: "A2A_USER_" + collider + ":n1", SessionID: collider + ":n1"}); err != nil || resp == nil || resp.Session == nil {
		t.Errorf("unrelated chat %q session reaped by %q's ReapNodeSessions (LIKE wildcard escaped?): %v", collider, target, err)
	}
}
