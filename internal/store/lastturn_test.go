package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestGetLastTurnWithContent_MatchesTailOfGetTurnsWithContent pins perf audit #3's
// correctness bar: the tail-only loader must return exactly what the last element of the
// full GetTurnsWithContent load would - same text, tokens, plan, and nodes.
func TestGetLastTurnWithContent_MatchesTailOfGetTurnsWithContent(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}

	for i := 0; i < 3; i++ {
		turnID := fmt.Sprintf("t%d", i)
		if err := st.SaveTurn(ctx, c.ID, turnID, fmt.Sprintf("question %d", i)); err != nil {
			t.Fatalf("SaveTurn %d: %v", i, err)
		}
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEvent(fmt.Sprintf("question %d", i))); err != nil {
			t.Fatalf("AppendEvent user %d: %v", i, err)
		}
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, asstEvent(&genai.Part{Text: fmt.Sprintf("answer %d", i)})); err != nil {
			t.Fatalf("AppendEvent asst %d: %v", i, err)
		}
		planID := fmt.Sprintf("p%d", i)
		if err := st.SaveDagPlan(ctx, c.ID, planID, turnID, "{}"); err != nil {
			t.Fatalf("SaveDagPlan %d: %v", i, err)
		}
		if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: planID, Status: "done"}); err != nil {
			t.Fatalf("UpsertDagNode %d: %v", i, err)
		}
	}

	full, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || len(full) != 3 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", full, err)
	}
	want := full[len(full)-1]

	got, err := st.GetLastTurnWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil {
		t.Fatalf("GetLastTurnWithContent: %v", err)
	}
	if got == nil {
		t.Fatal("GetLastTurnWithContent: nil, want the last turn")
	}
	if got.ID != want.ID || got.UserText != want.UserText || got.AsstText != want.AsstText {
		t.Errorf("GetLastTurnWithContent = %+v, want %+v", *got, want)
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Errorf("Nodes = %d, want %d", len(got.Nodes), len(want.Nodes))
	}
	if got.Plan == nil || want.Plan == nil || got.Plan.ID != want.Plan.ID {
		t.Errorf("Plan = %+v, want %+v", got.Plan, want.Plan)
	}
}

// TestGetLastTurnWithContent_TurnBiggerThanWindowStillComplete proves getLastTurnGroup's
// window-growing loop: a turn with more events than the starting NumRecentEvents window
// (lastTurnWindow=512) must still return its FULL text, not a truncated one.
func TestGetLastTurnWithContent_TurnBiggerThanWindowStillComplete(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t0", "go"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEvent("go")); err != nil {
		t.Fatalf("AppendEvent user: %v", err)
	}

	const n = 600 // > lastTurnWindow (512), forces at least one window-growth iteration
	var want string
	for i := 0; i < n; i++ {
		chunk := fmt.Sprintf("chunk%04d ", i)
		want += chunk
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, asstEvent(&genai.Part{Text: chunk})); err != nil {
			t.Fatalf("AppendEvent asst %d: %v", i, err)
		}
	}

	got, err := st.GetLastTurnWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil {
		t.Fatalf("GetLastTurnWithContent: %v", err)
	}
	if got == nil {
		t.Fatal("GetLastTurnWithContent: nil, want the turn")
	}
	if got.AsstText != want {
		t.Errorf("AsstText truncated: got %d chars, want %d chars", len(got.AsstText), len(want))
	}
}

// TestGetLastTurnWithContent_PartialRunInProgress covers a run still
// streaming: the ChatTurn row exists (SaveTurn runs before the model call
// starts) but no assistant event has landed yet. GetLastTurnWithContent must agree with GetTurnsWithContent - empty AsstText, not an error or a stale previous turn - since DeriveTerminalStatus reads exactly this state on every run-end call that races a still-draining stream.
func TestGetLastTurnWithContent_PartialRunInProgress(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t0", "still going"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEvent("still going")); err != nil {
		t.Fatalf("AppendEvent user: %v", err)
	}

	full, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || len(full) != 1 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", full, err)
	}
	want := full[0]

	got, err := st.GetLastTurnWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil {
		t.Fatalf("GetLastTurnWithContent: %v", err)
	}
	if got == nil {
		t.Fatal("GetLastTurnWithContent: nil, want the in-progress turn")
	}
	if got.ID != want.ID || got.UserText != want.UserText || got.AsstText != want.AsstText {
		t.Errorf("GetLastTurnWithContent = %+v, want %+v", *got, want)
	}
	if got.AsstText != "" {
		t.Errorf("AsstText = %q, want empty (no model response yet)", got.AsstText)
	}
}
