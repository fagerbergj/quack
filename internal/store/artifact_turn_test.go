package store

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"google.golang.org/adk/v2/artifact"
)

func TestTurnAwareService_SaveForTurn_StampsTurnID(t *testing.T) {
	st := newTestStore(t)
	row, err := NewRowArtifactService(st.db)
	if err != nil {
		t.Fatalf("NewRowArtifactService: %v", err)
	}
	w := NewTurnAwareService(row)
	ctx := context.Background()

	if _, err := w.SaveForTurn(ctx, &artifact.SaveRequest{
		AppName: "app", UserID: "u", SessionID: "chat-1", FileName: "f.txt", Part: mustPart("v1"),
	}, "turn-1"); err != nil {
		t.Fatalf("SaveForTurn: %v", err)
	}
	// A plain Save (the path ADK's own runner/tools use) leaves turn_id empty.
	if _, err := w.Save(ctx, &artifact.SaveRequest{
		AppName: "app", UserID: "u", SessionID: "chat-1", FileName: "g.txt", Part: mustPart("v1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	revs, err := w.RevisionsByTurn(ctx, "app", "u", "chat-1", "turn-1")
	if err != nil {
		t.Fatalf("RevisionsByTurn: %v", err)
	}
	if len(revs) != 1 || revs[0].Name != "f.txt" || revs[0].TurnID != "turn-1" {
		t.Fatalf("RevisionsByTurn(turn-1) = %+v, want exactly f.txt/turn-1", revs)
	}

	none, err := w.RevisionsByTurn(ctx, "app", "u", "chat-1", "turn-2")
	if err != nil {
		t.Fatalf("RevisionsByTurn(turn-2): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("RevisionsByTurn(turn-2) = %+v, want none (g.txt has no turn)", none)
	}
}

// artifact.InMemoryService() tracks no turn history - RevisionsByTurn must
// report that honestly (nil, no error) rather than panic or fake data.
func TestTurnAwareService_RevisionsByTurn_UnsupportedBackend(t *testing.T) {
	w := NewTurnAwareService(artifact.InMemoryService())
	revs, err := w.RevisionsByTurn(context.Background(), "app", "u", "s", "turn-1")
	if err != nil {
		t.Fatalf("RevisionsByTurn on in-memory backend: %v", err)
	}
	if revs != nil {
		t.Errorf("RevisionsByTurn on in-memory backend = %+v, want nil", revs)
	}
}

func TestDeleteChat_CascadesArtifacts(t *testing.T) {
	st := newTestStore(t)
	row, err := NewRowArtifactService(st.db)
	if err != nil {
		t.Fatalf("NewRowArtifactService: %v", err)
	}
	st.SetArtifactService(row)
	ctx := context.Background()

	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	userID := SessionUserFor(*chat)
	if _, err := row.Save(ctx, &artifact.SaveRequest{
		AppName: chatAppName, UserID: userID, SessionID: chat.ID, FileName: "photo.png", Part: mustPart("bytes"),
	}); err != nil {
		t.Fatalf("Save attachment: %v", err)
	}
	// A user-scoped artifact must survive - this chat doesn't own it.
	if _, err := row.Save(ctx, &artifact.SaveRequest{
		AppName: chatAppName, UserID: userID, SessionID: chat.ID, FileName: "user:pref.txt", Part: mustPart("x"),
	}); err != nil {
		t.Fatalf("Save user-scoped artifact: %v", err)
	}

	if err := st.DeleteChat(ctx, chat.ID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	if _, err := row.Load(ctx, &artifact.LoadRequest{AppName: chatAppName, UserID: userID, SessionID: chat.ID, FileName: "photo.png"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load chat-scoped attachment after DeleteChat = %v, want fs.ErrNotExist", err)
	}
	if _, err := row.Load(ctx, &artifact.LoadRequest{AppName: chatAppName, UserID: userID, SessionID: chat.ID, FileName: "user:pref.txt"}); err != nil {
		t.Errorf("Load user-scoped artifact after DeleteChat = %v, want it to survive", err)
	}
}

// DeleteChat with no artifact service wired must not panic or error.
func TestDeleteChat_NoArtifactService_NoOp(t *testing.T) {
	st := newTestStore(t)
	chat, err := st.CreateChat(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.DeleteChat(context.Background(), chat.ID); err != nil {
		t.Fatalf("DeleteChat with no artifact service: %v", err)
	}
}
