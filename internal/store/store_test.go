package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/store/storetest"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// New("sqlite", path) migrates both the app tables and ADK's session/event tables on pure-Go SQLite,
// and the app methods round-trip.
func TestSQLiteStoreRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	if st.Sessions == nil {
		t.Fatal("ADK session service is nil (session tables did not migrate)")
	}
	ctx := context.Background()

	c, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if got, err := st.GetChat(ctx, c.ID); err != nil || got.ID != c.ID || got.SystemPrompt != "sys" {
		t.Fatalf("GetChat: %+v err=%v", got, err)
	}
	if chats, _, err := st.ListChats(ctx, 0, "", ChatsScope{Active: true}); err != nil || len(chats) != 1 {
		t.Fatalf("ListChats: %d err=%v", len(chats), err)
	}

	// Turn + DAG plan + node round-trip.
	if err := st.SaveTurn(ctx, c.ID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	// The orchestrator's model + usage are stamped on the turn row at run end
	// (ADK's event storage drops ModelVersion) and must round-trip into TurnContent.
	if err := st.SetTurnUsage(ctx, c.ID, "t1", "gpt-oss-120b", TurnUsage{PromptTokens: 50, CompletionTokens: 10, CachedTokens: 5, TotalTokens: 60}); err != nil {
		t.Fatalf("SetTurnUsage: %v", err)
	}
	if turns, err := st.GetTurnsWithContent(ctx, "quack", "local", c.ID); err != nil || len(turns) != 1 || turns[0].Model != "gpt-oss-120b" || turns[0].CachedTokens != 5 {
		t.Fatalf("GetTurnsWithContent model/usage round-trip: %+v err=%v", turns, err)
	}
	if err := st.SaveDagPlan(ctx, c.ID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: "done", Output: "hi"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	if nodes, err := st.GetDagNodes(ctx, "p1"); err != nil || len(nodes) != 1 || nodes[0].Status != "done" {
		t.Fatalf("GetDagNodes: %+v err=%v", nodes, err)
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("sqlite file not created on disk: %v", err)
	}
}

// GetChatUsage/ChatsUsageTotals SUM a chat's plain-reply turns and DAG nodes in SQL; two chats prove the
// totals don't cross-contaminate.
func TestChatUsageAggregate(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	c1, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveTurn(ctx, c1.ID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.SetTurnUsage(ctx, c1.ID, "t1", "gpt-oss-120b", TurnUsage{
		PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, TotalTokens: 125, CachedTokens: 30,
	}); err != nil {
		t.Fatalf("SetTurnUsage: %v", err)
	}
	if err := st.SaveDagPlan(ctx, c1.ID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{
		NodeID: "n1", PlanID: "p1", Status: "done",
		PromptTokens: 200, CompletionTokens: 40, ReasoningTokens: 10, TotalTokens: 250, CachedTokens: 60,
	}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	// A second chat with its own DAG node - must not leak into c1's totals.
	c2, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat c2: %v", err)
	}
	if err := st.SaveTurn(ctx, c2.ID, "t2", ""); err != nil {
		t.Fatalf("SaveTurn c2: %v", err)
	}
	if err := st.SaveDagPlan(ctx, c2.ID, "p2", "t2", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan c2: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n2", PlanID: "p2", Status: "done", TotalTokens: 999}); err != nil {
		t.Fatalf("UpsertDagNode c2: %v", err)
	}

	agg, err := st.GetChatUsage(ctx, c1.ID)
	if err != nil {
		t.Fatalf("GetChatUsage: %v", err)
	}
	want := UsageAggregate{InputTokens: 300, OutputTokens: 60, ReasoningTokens: 15, CachedTokens: 90, TotalTokens: 375}
	if agg != want {
		t.Fatalf("GetChatUsage = %+v, want %+v", agg, want)
	}

	totals, err := st.ChatsUsageTotals(ctx, []string{c1.ID, c2.ID})
	if err != nil {
		t.Fatalf("ChatsUsageTotals: %v", err)
	}
	if totals[c1.ID] != 375 || totals[c2.ID] != 999 {
		t.Fatalf("ChatsUsageTotals = %+v, want c1=375 c2=999", totals)
	}

	// A chat with no turns/nodes yet must total 0, not error.
	c3, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat c3: %v", err)
	}
	if agg, err := st.GetChatUsage(ctx, c3.ID); err != nil || agg != (UsageAggregate{}) {
		t.Fatalf("GetChatUsage empty chat = %+v err=%v, want zero value", agg, err)
	}
}

// TestChatEventLog covers the durable event log backing SSE replay: ordered
// load, Last-Event-ID resume (afterSeq), per-run reset, and the windowing trim.
func TestChatEventLog(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	if err := st.db.Create(&Chat{ID: "c"}).Error; err != nil {
		t.Fatalf("create chat: %v", err)
	}

	for seq := int64(1); seq <= 4; seq++ {
		ev := ChatEvent{ChatID: "c", Seq: seq, Event: `{"name":"node_start"}`}
		if err := st.InsertChatEvent(ctx, ev); err != nil {
			t.Fatalf("InsertChatEvent %d: %v", seq, err)
		}
	}

	// Full replay, ordered by seq.
	evs, err := st.LoadChatEvents(ctx, "c", 0)
	if err != nil || len(evs) != 4 {
		t.Fatalf("LoadChatEvents(0): %d err=%v", len(evs), err)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d (not ordered)", i, e.Seq, i+1)
		}
	}

	// Resume from seq 2 → only events 3 and 4.
	if evs, err := st.LoadChatEvents(ctx, "c", 2); err != nil || len(evs) != 2 || evs[0].Seq != 3 {
		t.Fatalf("LoadChatEvents(2): %+v err=%v, want seqs [3,4]", evs, err)
	}

	// Trim windows away the oldest.
	if err := st.TrimChatEvents(ctx, "c", 2); err != nil {
		t.Fatalf("TrimChatEvents: %v", err)
	}
	if evs, err := st.LoadChatEvents(ctx, "c", 0); err != nil || len(evs) != 2 || evs[0].Seq != 3 {
		t.Fatalf("after trim: %+v err=%v, want seqs [3,4]", evs, err)
	}

	// Reset clears the chat (a new run starts fresh); another chat is untouched.
	if err := st.db.Create(&Chat{ID: "other"}).Error; err != nil {
		t.Fatalf("create chat other: %v", err)
	}
	if err := st.InsertChatEvent(ctx, ChatEvent{ChatID: "other", Seq: 1, Event: "{}"}); err != nil {
		t.Fatalf("InsertChatEvent other: %v", err)
	}
	if err := st.DeleteChatEvents(ctx, "c"); err != nil {
		t.Fatalf("DeleteChatEvents: %v", err)
	}
	if evs, err := st.LoadChatEvents(ctx, "c", 0); err != nil || len(evs) != 0 {
		t.Fatalf("after reset: %d events, want 0 (err=%v)", len(evs), err)
	}
	if evs, err := st.LoadChatEvents(ctx, "other", 0); err != nil || len(evs) != 1 {
		t.Fatalf("other chat clobbered: %d, want 1 (err=%v)", len(evs), err)
	}
}

// SetChatGitHub creates a missing row (the webhook can precede the chat) and updates an existing one, but
// never moves SessionUser: existing session history was written under the original login.
func TestSetChatGitHub(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	id := "github-acme-widget-app-42"
	if err := st.SetChatGitHub(ctx, id, "acme/widget-app", "https://github.com/acme/widget-app/issues/42", "", "alice"); err != nil {
		t.Fatalf("SetChatGitHub (create): %v", err)
	}
	got, err := st.GetChat(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetChat: %+v err=%v", got, err)
	}
	if got.GithubRepo != "acme/widget-app" || got.GithubURL != "https://github.com/acme/widget-app/issues/42" {
		t.Fatalf("unexpected github fields: %+v", got)
	}
	if got.SessionUser != "alice" {
		t.Fatalf("SessionUser = %q, want %q", got.SessionUser, "alice")
	}

	// A second call by a different commenter updates the github fields in place and keeps SessionUser.
	if err := st.SetChatGitHub(ctx, id, "acme/widget-app", "https://github.com/acme/widget-app/pull/42", "", "bob"); err != nil {
		t.Fatalf("SetChatGitHub (update): %v", err)
	}
	got, err = st.GetChat(ctx, id)
	if err != nil || got.GithubURL != "https://github.com/acme/widget-app/pull/42" {
		t.Fatalf("update did not take: %+v err=%v", got, err)
	}
	if got.SessionUser != "alice" {
		t.Fatalf("SessionUser after update = %q, want unchanged %q", got.SessionUser, "alice")
	}
	if chats, _, err := st.ListChats(ctx, 0, "", ChatsScope{Active: true}); err != nil || len(chats) != 1 {
		t.Fatalf("ListChats: %d err=%v (update must not create a duplicate row)", len(chats), err)
	}
}

func TestStoreUnknownKind(t *testing.T) {
	if _, err := New("mysql", "x"); err == nil {
		t.Error("New should reject an unknown store kind")
	}
}

func userEvent(text string) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "user"
	ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
	return ev
}

func asstEvent(parts ...*genai.Part) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "orchestrator"
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	return ev
}

// orchestratorAgentNodeEvent is the orchestrator's own reply as ADK stamps it: AgentNode-wrapped, so it
// carries NodeInfo too. Author, not NodeInfo, distinguishes it from a gate-internal node's event.
func orchestratorAgentNodeEvent(text string) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "orchestrator"
	ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}
	ev.NodeInfo = &session.NodeInfo{Path: "orchestrator-workflow@1/orchestrator@1"}
	return ev
}

// answerEvent is how a resumed clarification answer is persisted: a user-authored
// event whose only part is a get_user_choice FunctionResponse carrying the choice.
func answerEvent(choice string) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "user"
	ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "get_user_choice", Response: map[string]any{"choice": choice}},
	}}}
	return ev
}

// TestGroupSessionEvents verifies per-turn bucketing, text/thinking split, and
// tool-call extraction with result pairing and transfer_to_agent exclusion.
func TestGroupSessionEvents(t *testing.T) {
	events := []*session.Event{
		userEvent("which Springfield?"),
		asstEvent(&genai.Part{Text: "deciding…", Thought: true}),
		asstEvent(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "get_user_choice", Args: map[string]any{"options": []any{"IL", "MO"}}}}),
		asstEvent(&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "get_user_choice", Response: map[string]any{"status": "pending"}}}),
		// transfer_to_agent is noise and must be dropped.
		asstEvent(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "t1", Name: transferTool, Args: map[string]any{}}}),
		// The answer arrives as a get_user_choice FunctionResponse on a user event.
		answerEvent("IL"),
		asstEvent(&genai.Part{Text: "Springfield, Illinois has…"}),
	}

	groups := groupSessionEvents(slices.Values(events))

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	g0 := groups[0]
	if g0.userText.String() != "which Springfield?" {
		t.Errorf("turn 0 userText = %q", g0.userText.String())
	}
	if g0.asstThink.String() != "deciding…" {
		t.Errorf("turn 0 asstThink = %q", g0.asstThink.String())
	}
	if len(g0.toolCalls) != 1 {
		t.Fatalf("turn 0 toolCalls = %d, want 1 (transfer_to_agent excluded)", len(g0.toolCalls))
	}
	tc := g0.toolCalls[0]
	if tc.CallID != "c1" || tc.Name != "get_user_choice" {
		t.Errorf("toolCall = %+v", tc)
	}
	if tc.Result == nil || tc.Result["status"] != "pending" {
		t.Errorf("toolCall result not paired: %+v", tc.Result)
	}

	g1 := groups[1]
	if g1.userText.String() != "IL" || g1.asstText.String() != "Springfield, Illinois has…" {
		t.Errorf("turn 1 = %+v", g1)
	}
	if len(g1.toolCalls) != 0 {
		t.Errorf("turn 1 toolCalls = %d, want 0", len(g1.toolCalls))
	}
}

// compactionEvent mirrors ADK's persisted compaction summary: Author "user" but no top-level Content (the
// prose lives under Actions.Compaction). groupSessionEvents must not read it as a turn boundary.
func compactionEvent() *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "user"
	ev.Actions.Compaction = &session.EventCompaction{
		CompactedContent: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "earlier turns summarized…"}}},
	}
	return ev
}

// A compaction summary must not register as a turn boundary, or groupSessionEvents' length desyncs from
// the turn rows and every later turn's content shifts onto the wrong ChatTurn.
func TestGroupSessionEvents_CompactionEventDoesNotSplitATurn(t *testing.T) {
	events := []*session.Event{
		userEvent("turn one"),
		asstEvent(&genai.Part{Text: "reply one"}),
		compactionEvent(),
		userEvent("turn two"),
		asstEvent(&genai.Part{Text: "reply two"}),
	}

	groups := groupSessionEvents(slices.Values(events))

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (the compaction event must not start a third)", len(groups))
	}
	if groups[0].userText.String() != "turn one" || groups[0].asstText.String() != "reply one" {
		t.Errorf("turn 0 = %+v", groups[0])
	}
	if groups[1].userText.String() != "turn two" || groups[1].asstText.String() != "reply two" {
		t.Errorf("turn 1 = %+v", groups[1])
	}
}

// nodeEvent is a gate-internal event (a worker draft, an advisor consult), tagged with NodeInfo: never the
// user-facing message.
func nodeEvent(path string, parts ...*genai.Part) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "web-researcher"
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	ev.NodeInfo = &session.NodeInfo{Path: path}
	return ev
}

// Gate-internal events (NodeInfo set) contribute neither asstText nor toolCalls, so a node's deliberation
// never shows as the turn's message.
func TestGroupSessionEvents_NodeActivityExcluded(t *testing.T) {
	events := []*session.Event{
		userEvent("research X"),
		asstEvent(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "e1", Name: "execute", Args: map[string]any{"plan_id": "p1"}}}),
		// Gate-internal: an advisor consult and a worker draft, both node-scoped.
		nodeEvent("n1/advisor-r0@1", &genai.Part{Text: "Consider checking multiple sources."}),
		nodeEvent("n1/worker-r0@1", &genai.Part{FunctionCall: &genai.FunctionCall{ID: "w1", Name: "web_search", Args: map[string]any{"query": "X"}}}),
		nodeEvent("n1/worker-r0@1", &genai.Part{Text: "raw unvetted draft text"}),
		// The real delivered answer: a top-level orchestrator event (persistAnswer).
		asstEvent(&genai.Part{Text: "The real, vetted answer."}),
	}

	groups := groupSessionEvents(slices.Values(events))
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]
	if g.asstText.String() != "The real, vetted answer." {
		t.Errorf("asstText = %q, want only the top-level answer (no node-scoped leak)", g.asstText.String())
	}
	for _, tc := range g.toolCalls {
		if tc.Name == "web_search" {
			t.Errorf("toolCalls includes a node-scoped call: %+v", tc)
		}
	}
	if len(g.toolCalls) != 1 || g.toolCalls[0].Name != "execute" {
		t.Errorf("toolCalls = %+v, want only the top-level execute call", g.toolCalls)
	}
}

// The orchestrator's own model events' UsageMetadata sums per turn; a gate-internal node's usage (surfaced
// via DagNodeState) must not leak in.
func TestGroupSessionEvents_UsageAccumulation(t *testing.T) {
	orch1 := asstEvent(&genai.Part{Text: "thinking"})
	orch1.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 30, CandidatesTokenCount: 5}
	orch2 := asstEvent(&genai.Part{Text: "The answer."})
	orch2.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 40, CandidatesTokenCount: 15, ThoughtsTokenCount: 2}

	node := nodeEvent("n1/worker-r0@1", &genai.Part{Text: "raw draft"})
	node.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 999, CandidatesTokenCount: 999}

	events := []*session.Event{
		userEvent("research X"),
		orch1,
		node,
		orch2,
	}

	groups := groupSessionEvents(slices.Values(events))
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]
	if g.promptTokens != 70 || g.completionTokens != 20 || g.reasoningTokens != 2 {
		t.Errorf("usage = prompt=%d completion=%d reasoning=%d, want 70/20/2 (node-scoped usage must not leak in)",
			g.promptTokens, g.completionTokens, g.reasoningTokens)
	}
}

// A turn without SetTurnUsage's stamp falls back to the session-walk usage for all five token fields.
func TestGetTurnsWithContent_UsageFallbackIsSymmetric(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	// SaveTurn only - no SetTurnUsage, so the ChatTurn row's token columns
	// stay zero and GetTurnsWithContent must fall back to the session walk.
	if err := st.SaveTurn(ctx, c.ID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}

	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	userEv := session.NewEvent(ctx, "test")
	userEv.Author = "user"
	userEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEv); err != nil {
		t.Fatalf("AppendEvent user: %v", err)
	}
	asstEv := session.NewEvent(ctx, "test")
	asstEv.Author = orchestratorAuthor
	asstEv.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "hello"}}}
	asstEv.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 50, CandidatesTokenCount: 10, ThoughtsTokenCount: 2, TotalTokenCount: 62, CachedContentTokenCount: 15,
	}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, asstEv); err != nil {
		t.Fatalf("AppendEvent asst: %v", err)
	}

	turns, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", turns, err)
	}
	tc := turns[0]
	if tc.PromptTokens != 50 || tc.CompletionTokens != 10 || tc.ReasoningTokens != 2 ||
		tc.TotalTokens != 62 || tc.CachedTokens != 15 {
		t.Errorf("fallback usage = %+v, want prompt=50 completion=10 reasoning=2 total=62 cached=15 (all five fields from the session walk)", tc)
	}
}

// A ResetHistory dispatch deletes the whole ADK session; earlier turns must still show the user's text
// from ChatTurn.UserText instead of empty content.
func TestGetTurnsWithContent_SurvivesSessionReset(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	const chatID = "ext:github:c1"
	if err := st.db.Create(&Chat{ID: chatID}).Error; err != nil {
		t.Fatalf("create chat: %v", err)
	}

	// Turn 1: runs normally, with a real session event.
	if err := st.SaveTurn(ctx, chatID, "t1", "first review"); err != nil {
		t.Fatalf("SaveTurn t1: %v", err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: chatID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	userEv := session.NewEvent(ctx, "test")
	userEv.Author = "user"
	userEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "first review"}}}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEv); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// Reset deletes the session before the new turn's events land. ADK's events cascade on session delete, but
	// SQLite enforces that per-connection, so delete the orphaned event row directly to stay deterministic.
	if err := st.Sessions.Delete(ctx, &session.DeleteRequest{AppName: chatAppName, UserID: "local", SessionID: chatID}); err != nil {
		t.Fatalf("session Delete: %v", err)
	}
	if err := st.db.Exec("DELETE FROM events WHERE session_id = ?", chatID).Error; err != nil {
		t.Fatalf("delete orphaned events: %v", err)
	}
	if err := st.SaveTurn(ctx, chatID, "t2", "re-review, new commits"); err != nil {
		t.Fatalf("SaveTurn t2: %v", err)
	}
	sessResp, err = st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "local", SessionID: chatID})
	if err != nil {
		t.Fatalf("session re-Create: %v", err)
	}
	userEv2 := session.NewEvent(ctx, "test")
	userEv2.Author = "user"
	userEv2.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "re-review, new commits"}}}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, userEv2); err != nil {
		t.Fatalf("AppendEvent t2: %v", err)
	}

	turns, err := st.GetTurnsWithContent(ctx, chatAppName, "local", chatID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v, want 2 turns", turns, err)
	}
	if turns[0].UserText != "first review" {
		t.Errorf("turn 0 (predates the reset) UserText = %q, want %q from the stored fallback", turns[0].UserText, "first review")
	}
	if turns[1].UserText != "re-review, new commits" {
		t.Errorf("turn 1 UserText = %q, want %q", turns[1].UserText, "re-review, new commits")
	}
}

// DeleteChat must reap everything keyed off the chat id: turns, DAG state, event log and ADK session. A
// GitHub chat with no recorded SessionUser has its session under fallback user "github".
func TestDeleteChat_ReapsSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	const chatID = "github-acme-widget-app-9"
	if err := st.SetChatGitHub(ctx, chatID, "acme/widget-app", "https://github.com/acme/widget-app/pull/9", "", ""); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	if err := st.SaveTurn(ctx, chatID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.SaveDagPlan(ctx, chatID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: "done"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	if err := st.InsertChatEvent(ctx, ChatEvent{ChatID: chatID, Seq: 1, Event: "{}"}); err != nil {
		t.Fatalf("InsertChatEvent: %v", err)
	}
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "github", SessionID: chatID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, session.NewEvent(ctx, "test")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if err := st.DeleteChat(ctx, chatID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	if got, err := st.GetChat(ctx, chatID); err != nil || got != nil {
		t.Errorf("GetChat after delete = %+v err=%v, want nil,nil", got, err)
	}
	var turnCount, planCount, nodeCount, eventCount int64
	st.db.Model(&ChatTurn{}).Where("chat_id = ?", chatID).Count(&turnCount)
	st.db.Model(&DagPlan{}).Where("chat_id = ?", chatID).Count(&planCount)
	st.db.Model(&DagNode{}).Where("plan_id = ?", "p1").Count(&nodeCount)
	st.db.Model(&ChatEvent{}).Where("chat_id = ?", chatID).Count(&eventCount)
	if turnCount != 0 || planCount != 0 || nodeCount != 0 || eventCount != 0 {
		t.Errorf("orphaned rows after delete: turns=%d plans=%d nodes=%d events=%d, want all 0",
			turnCount, planCount, nodeCount, eventCount)
	}

	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: chatAppName, UserID: "github", SessionID: chatID}); err == nil && resp != nil && resp.Session != nil {
		t.Errorf("ADK session still present after DeleteChat, want reaped: %+v", resp.Session)
	}
}

// A GitHub chat for "alice" resolves "alice" via SessionUserFor and SessionUserForChat, and DeleteChat reaps
// under it. Unrecorded GitHub chats fall back to "github"; non-GitHub chats to "local".
func TestSessionUserForChat_RoundTrip(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	const chatID = "github-acme-widget-app-11"
	if err := st.SetChatGitHub(ctx, chatID, "acme/widget-app", "https://github.com/acme/widget-app/pull/11", "", "alice"); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	if got := st.SessionUserForChat(ctx, chatID); got != "alice" {
		t.Errorf("SessionUserForChat(alice-chat) = %q, want %q", got, "alice")
	}

	// The write side (an ADK session created under "alice") must be exactly
	// what DeleteChat reaps.
	sessResp, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: chatAppName, UserID: "alice", SessionID: chatID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	if err := st.Sessions.AppendEvent(ctx, sessResp.Session, session.NewEvent(ctx, "test")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := st.DeleteChat(ctx, chatID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: chatAppName, UserID: "alice", SessionID: chatID}); err == nil && resp != nil && resp.Session != nil {
		t.Errorf("ADK session still present under alice after DeleteChat, want reaped: %+v", resp.Session)
	}

	// Older chat, no recorded SessionUser: falls back to id-shape default.
	const oldChatID = "github-acme-widget-app-12"
	if err := st.SetChatGitHub(ctx, oldChatID, "acme/widget-app", "https://github.com/acme/widget-app/pull/12", "", ""); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	if got := st.SessionUserForChat(ctx, oldChatID); got != "github" {
		t.Errorf("SessionUserForChat(unrecorded github chat) = %q, want fallback %q", got, "github")
	}

	// Non-GitHub chat, id-shape fallback.
	if got := st.SessionUserForChat(ctx, "not-a-chat-id"); got != "local" {
		t.Errorf("SessionUserForChat(unknown chat) = %q, want fallback %q", got, "local")
	}
}

// The orchestrator's own (no-DAG) reply carries NodeInfo but must still be captured: only different-author
// events are excluded.
func TestGroupSessionEvents_OrchestratorOwnReplyKept(t *testing.T) {
	events := []*session.Event{
		userEvent("what is the tallest mountain?"),
		orchestratorAgentNodeEvent("Mount Everest, per National Geographic."),
	}
	groups := groupSessionEvents(slices.Values(events))
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if got := groups[0].asstText.String(); got != "Mount Everest, per National Geographic." {
		t.Errorf("asstText = %q, want the orchestrator's own reply preserved", got)
	}
}

// Every persisted title is capped once here, whatever the caller (titler, manual rename, origin label).
func TestUpdateTitle_CapsLength(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	longAnswer := "# WAL Design Comparison: PostgreSQL vs. SQLite\n\n## Researcher Node A — " + strings.Repeat("detailed findings ", 50)
	if err := st.UpdateTitle(ctx, c.ID, longAnswer); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	got, err := st.GetChat(ctx, c.ID)
	if err != nil || got == nil {
		t.Fatalf("GetChat: %v, %v", got, err)
	}
	if n := len([]rune(got.Title)); n > MaxTitleLen {
		t.Fatalf("Title length = %d, want <= MaxTitleLen (%d)", n, MaxTitleLen)
	}
	if got.Title == longAnswer {
		t.Fatalf("Title = the full uncapped answer, want it truncated")
	}
}

// TestTruncateTitle_WordBoundary: a long PR title must not be cut
// mid-word with no indication it was shortened.
func TestTruncateTitle_WordBoundary(t *testing.T) {
	title := "fix(ledger,vetting): one representation per fact - delivery and judge rounds (#1230)"
	got := truncateTitle(title, MaxTitleLen)
	if n := len([]rune(got)); n > MaxTitleLen {
		t.Fatalf("len = %d, want <= %d", n, MaxTitleLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q, want an ellipsis suffix", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "(#1") {
		t.Fatalf("got %q, cut mid-word instead of at a space", got)
	}
	if short := "short title"; truncateTitle(short, MaxTitleLen) != short {
		t.Fatalf("truncateTitle changed a title already under the cap")
	}
}

// A chat's ledger_checkpoints row must not survive DeleteChat: chat ids never repeat, so it would only leak.
func TestDeleteChat_RemovesCheckpointRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.upsertCheckpoint(ctx, chat.ID, 1, []byte(`{}`)); err != nil {
		t.Fatalf("upsertCheckpoint: %v", err)
	}

	if err := st.DeleteChat(ctx, chat.ID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	var count int64
	st.db.Model(&Checkpoint{}).Where("chat_id = ?", chat.ID).Count(&count)
	if count != 0 {
		t.Errorf("ledger_checkpoints rows for %s after delete = %d, want 0", chat.ID, count)
	}
}

// dag_nodes are fetched with one plan_id IN (?) query, so the query delta between 5 and 20 turns
// (each with its own plan+node) is constant.
func TestGetTurnsWithContent_NodesQueryIsConstant(t *testing.T) {
	ctx := context.Background()
	run := func(n int) int64 {
		st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		c, err := st.CreateChat(ctx, "")
		if err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		for i := 0; i < n; i++ {
			turnID := "t" + string(rune('a'+i))
			if err := st.SaveTurn(ctx, c.ID, turnID, ""); err != nil {
				t.Fatalf("SaveTurn: %v", err)
			}
			planID := "p" + string(rune('a'+i))
			if err := st.SaveDagPlan(ctx, c.ID, planID, turnID, "{}"); err != nil {
				t.Fatalf("SaveDagPlan: %v", err)
			}
			if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: planID, Status: "done"}); err != nil {
				t.Fatalf("UpsertDagNode: %v", err)
			}
		}
		queries := storetest.RecordQueries(t, st.DB())
		if _, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID); err != nil {
			t.Fatalf("GetTurnsWithContent: %v", err)
		}
		return int64(len(queries()))
	}

	q5 := run(5)
	q20 := run(20)
	if q5 != q20 {
		t.Errorf("query delta = %d at N=5, %d at N=20, want equal (constant, not N+1)", q5, q20)
	}
}

// A plan mid-execution (done, running, not started) still folds into one DagNode per node: the running
// chat's UI must not wait for the whole plan before any node card appears.
func TestGetTurnsWithContent_RunningPlanShowsOneCardPerNode(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.SaveDagPlan(ctx, c.ID, "p1", "t1", `{"plan_id":"p1"}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	for _, n := range []DagNode{
		{NodeID: "code-implementer-1", PlanID: "p1", Status: "done"},
		{NodeID: "code-reviewer-1", PlanID: "p1", Status: "running"},
		{NodeID: "synthesizer-1", PlanID: "p1", Status: "queued"},
	} {
		if err := st.UpsertDagNode(ctx, n); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", n.NodeID, err)
		}
	}

	turns, err := st.GetTurnsWithContent(ctx, chatAppName, "local", c.ID)
	if err != nil {
		t.Fatalf("GetTurnsWithContent: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if turns[0].Plan == nil {
		t.Fatal("turns[0].Plan is nil - a running plan must still show up")
	}
	if len(turns[0].Nodes) != 3 {
		t.Fatalf("turns[0].Nodes = %+v, want one card per node (3) regardless of the plan's own completion state", turns[0].Nodes)
	}
	gotStatus := map[string]string{}
	for _, n := range turns[0].Nodes {
		gotStatus[n.NodeID] = n.Status
	}
	for id, want := range map[string]string{"code-implementer-1": "done", "code-reviewer-1": "running", "synthesizer-1": "queued"} {
		if gotStatus[id] != want {
			t.Errorf("node %s status = %q, want %q", id, gotStatus[id], want)
		}
	}
}
