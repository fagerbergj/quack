package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
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

// TestTurnContent_DeliveredAnswerSurvivesStorage: the orchestrator's delivered-answer message
// is told apart from its narration after a round trip through the session store.
func TestTurnContent_DeliveredAnswerSurvivesStorage(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t1", "q"); err != nil {
		t.Fatal(err)
	}
	// Built in append order: the store orders events by their creation timestamps.
	for _, ev := range []*session.Event{userEvent("q"), asstEvent(&genai.Part{Text: "planning chatter. "}), deliveredEvent("THE DELIVERED ANSWER")} {
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || len(turns) != 1 || turns[0].Answer != "THE DELIVERED ANSWER" {
		t.Fatalf("turns = %+v err=%v, want the delivered answer apart from the narration", turns, err)
	}
}

func deliveredEvent(text string) *session.Event { return deliveredFor("", text) }

func deliveredFor(turnID, text string) *session.Event {
	ev := asstEvent(&genai.Part{Text: text})
	ev.CustomMetadata = map[string]any{stream.DeliveredAnswerMeta: turnID, stream.DeliveredAtMeta: ev.Timestamp.UTC().Format(time.RFC3339Nano)}
	return ev
}

// spaced gives events strictly increasing, whole-second timestamps, so their stored order can't tie.
func spaced(evs ...*session.Event) []*session.Event {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i, ev := range evs {
		ev.Timestamp = base.Add(time.Duration(i) * time.Second)
		if _, ok := ev.CustomMetadata[stream.DeliveredAtMeta]; ok {
			ev.CustomMetadata[stream.DeliveredAtMeta] = ev.Timestamp.UTC().Format(time.RFC3339Nano)
		}
	}
	return evs
}

// TestTurnContent_DeliveredAnswersAttachToTheirTurn: a retry or boot resume appends its answer
// with no user event of its own; it attaches to its plan's turn, replacing (not joining) the
// earlier answer, and never lands in a later chat-only turn.
func TestTurnContent_DeliveredAnswersAttachToTheirTurn(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := st.SaveTurn(ctx, c.ID, id, "q"); err != nil {
			t.Fatal(err)
		}
	}
	for _, ev := range spaced(
		userEvent("plan it"), deliveredFor("t1", "ANSWER-1"),
		userEvent("just chat"), asstEvent(&genai.Part{Text: "T2 REPLY"}),
		deliveredFor("t1", "RETRY-A"), deliveredFor("t1", "RETRY-B"), // a retry, then a resume of a second sibling
	) {
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns = %+v err=%v", turns, err)
	}
	if turns[0].Answer != "RETRY-B" {
		t.Errorf("t1 answer = %q, want the latest delivery RETRY-B alone", turns[0].Answer)
	}
	if turns[1].Answer != "" || turns[1].AsstText != "T2 REPLY" {
		t.Errorf("t2 = answer %q, text %q; want only its own reply", turns[1].Answer, turns[1].AsstText)
	}
}

// TestGroupSessionEvents_LatestDeliveryWinsOnTiedTimestamps: two answers for one turn whose
// stored timestamps tie (and load in either order) resolve by their own delivery times.
func TestGroupSessionEvents_LatestDeliveryWinsOnTiedTimestamps(t *testing.T) {
	tie := time.Now().Truncate(time.Millisecond)
	mk := func(text string, deliveredAt time.Time) *session.Event {
		ev := deliveredFor("t1", text)
		ev.Timestamp = tie
		ev.CustomMetadata[stream.DeliveredAtMeta] = deliveredAt.UTC().Format(time.RFC3339Nano)
		return ev
	}
	early, late := mk("EARLY", tie), mk("LATE", tie.Add(time.Microsecond))
	for _, order := range [][]*session.Event{{early, late}, {late, early}} {
		groups := groupSessionEvents(slices.Values(append([]*session.Event{userEvent("q")}, order...)))
		if got := keyedAnswers(groups)["t1"].text; got != "LATE" {
			t.Errorf("answer = %q, want the later delivery whatever the load order", got)
		}
	}
}

// TestGetLastTurnWithContent_ReadsDeliveredAnswer: the last-turn loader takes the turn's
// delivered answer (and its retry replacements) the same way the full loader does.
func TestGetLastTurnWithContent_ReadsDeliveredAnswer(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t1", "q"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range spaced(userEvent("q"), deliveredFor("t1", "FIRST"), deliveredFor("t1", "RETRIED")) {
		if err := st.Sessions.AppendEvent(ctx, sessResp.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	last, err := st.GetLastTurnWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || last == nil || last.Answer != "RETRIED" {
		t.Fatalf("last turn = %+v err=%v, want answer RETRIED", last, err)
	}
}
