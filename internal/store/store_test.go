package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestSQLiteStoreRoundTrip proves the dialector swap: New("sqlite", path) migrates
// both the app tables AND the ADK session/event tables on a pure-Go SQLite file,
// and the app methods round-trip. Runs cgo-free (modernc driver).
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
	if chats, _, err := st.ListChats(ctx, 0, ""); err != nil || len(chats) != 1 {
		t.Fatalf("ListChats: %d err=%v", len(chats), err)
	}

	// Turn + DAG plan + node round-trip.
	if err := st.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	// The orchestrator's model is stamped on the turn row at run end (ADK's
	// event storage drops ModelVersion) and must round-trip into TurnContent.
	if err := st.SetTurnModel(ctx, c.ID, "t1", "gpt-oss-120b"); err != nil {
		t.Fatalf("SetTurnModel: %v", err)
	}
	if turns, err := st.GetTurnsWithContent(ctx, "quack", "local", c.ID); err != nil || len(turns) != 1 || turns[0].Model != "gpt-oss-120b" {
		t.Fatalf("GetTurnsWithContent model round-trip: %+v err=%v", turns, err)
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

// TestChatEventLog covers the durable event log backing SSE replay: ordered
// load, Last-Event-ID resume (afterSeq), per-run reset, and the windowing trim.
func TestChatEventLog(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

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

// TestSetChatGitHub covers both the create-if-missing path (webhook dispatch
// can fire before the chat row exists) and updating an existing row. Also
// pins #512's read/write asymmetry fix: SessionUser is recorded on create and
// must NOT move on a later dispatch by a different commenter, since existing
// session history was already written under the original login.
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

	// Second call (row now exists, different commenter) must update the
	// github fields in place, not error/duplicate - and must NOT move
	// SessionUser off the login the existing session was written under.
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
	if chats, _, err := st.ListChats(ctx, 0, ""); err != nil || len(chats) != 1 {
		t.Fatalf("ListChats: %d err=%v (update must not create a duplicate row)", len(chats), err)
	}
}

// TestGithubSnapshotRoundTrip pins the #459 snapshot store: no row yet reads
// as (ok=false, no error), a set is readable back verbatim, and a second set
// updates in place (one row per chat, not a growing history).
func TestGithubSnapshotRoundTrip(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	id := "github-acme-widgets-7"

	if _, ok, err := st.GetGithubSnapshot(ctx, id); err != nil || ok {
		t.Fatalf("GetGithubSnapshot before any Set: ok=%v err=%v; want (false, nil)", ok, err)
	}

	if err := st.SetGithubSnapshot(ctx, id, `{"title":"v1"}`); err != nil {
		t.Fatalf("SetGithubSnapshot (create): %v", err)
	}
	got, ok, err := st.GetGithubSnapshot(ctx, id)
	if err != nil || !ok || got != `{"title":"v1"}` {
		t.Fatalf("GetGithubSnapshot after create: got=%q ok=%v err=%v", got, ok, err)
	}

	if err := st.SetGithubSnapshot(ctx, id, `{"title":"v2"}`); err != nil {
		t.Fatalf("SetGithubSnapshot (update): %v", err)
	}
	got, ok, err = st.GetGithubSnapshot(ctx, id)
	if err != nil || !ok || got != `{"title":"v2"}` {
		t.Fatalf("GetGithubSnapshot after update: got=%q ok=%v err=%v", got, ok, err)
	}
}

// TestGithubReviewBaselineRoundTrip pins the #459 follow-up fix's store half:
// separate from GithubSnapshot, no row until explicitly set, then readable
// back verbatim and updatable in place.
func TestGithubReviewBaselineRoundTrip(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	id := "github-acme-widgets-7"

	if _, ok, err := st.GetGithubReviewBaseline(ctx, id); err != nil || ok {
		t.Fatalf("GetGithubReviewBaseline before any Set: ok=%v err=%v; want (false, nil)", ok, err)
	}

	if err := st.SetGithubReviewBaseline(ctx, id, `["pid1"]`); err != nil {
		t.Fatalf("SetGithubReviewBaseline (create): %v", err)
	}
	got, ok, err := st.GetGithubReviewBaseline(ctx, id)
	if err != nil || !ok || got != `["pid1"]` {
		t.Fatalf("GetGithubReviewBaseline after create: got=%q ok=%v err=%v", got, ok, err)
	}

	if err := st.SetGithubReviewBaseline(ctx, id, `["pid1","pid2"]`); err != nil {
		t.Fatalf("SetGithubReviewBaseline (update): %v", err)
	}
	got, ok, err = st.GetGithubReviewBaseline(ctx, id)
	if err != nil || !ok || got != `["pid1","pid2"]` {
		t.Fatalf("GetGithubReviewBaseline after update: got=%q ok=%v err=%v", got, ok, err)
	}

	// Independent of GithubSnapshot - setting one must not create/affect the other.
	if _, ok, err := st.GetGithubSnapshot(ctx, id); err != nil || ok {
		t.Fatalf("GetGithubSnapshot should be untouched by SetGithubReviewBaseline: ok=%v err=%v", ok, err)
	}
}

// TestGithubMergeIntentRoundTrip pins the standing quack:merge intent's store
// half: no row until explicitly set, then readable back with the recorder,
// updatable in place (re-applying the label refreshes it), and gone after
// delete (consumed by a merge).
func TestGithubMergeIntentRoundTrip(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	id := "github-acme-widgets-7"

	if got, err := st.GetGithubMergeIntent(ctx, id); err != nil || got != nil {
		t.Fatalf("GetGithubMergeIntent before any Set: got=%v err=%v; want (nil, nil)", got, err)
	}

	if err := st.SetGithubMergeIntent(ctx, id, "alice"); err != nil {
		t.Fatalf("SetGithubMergeIntent (create): %v", err)
	}
	got, err := st.GetGithubMergeIntent(ctx, id)
	if err != nil || got == nil || got.RequestedBy != "alice" {
		t.Fatalf("GetGithubMergeIntent after create: got=%+v err=%v", got, err)
	}

	// A second application (by someone else) refreshes the authorizer in place.
	if err := st.SetGithubMergeIntent(ctx, id, "bob"); err != nil {
		t.Fatalf("SetGithubMergeIntent (update): %v", err)
	}
	got, err = st.GetGithubMergeIntent(ctx, id)
	if err != nil || got == nil || got.RequestedBy != "bob" {
		t.Fatalf("GetGithubMergeIntent after update: got=%+v err=%v", got, err)
	}

	if err := st.DeleteGithubMergeIntent(ctx, id); err != nil {
		t.Fatalf("DeleteGithubMergeIntent: %v", err)
	}
	if got, err := st.GetGithubMergeIntent(ctx, id); err != nil || got != nil {
		t.Fatalf("GetGithubMergeIntent after delete: got=%v err=%v; want (nil, nil)", got, err)
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

// orchestratorAgentNodeEvent is the orchestrator's OWN reply as ADK actually
// stamps it in production: the orchestrator llmagent is wrapped in a
// workflow.AgentNode too (Start → agentNode), so its real events carry NodeInfo
// just like a gate-internal node's - "author, not NodeInfo" is what distinguishes
// them. Regression fixture for the bug where a plain `NodeInfo != nil` exclusion
// filter dropped the orchestrator's own conversational (no-DAG) answer entirely.
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
	if g0.userText != "which Springfield?" {
		t.Errorf("turn 0 userText = %q", g0.userText)
	}
	if g0.asstThink != "deciding…" {
		t.Errorf("turn 0 asstThink = %q", g0.asstThink)
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
	if g1.userText != "IL" || g1.asstText != "Springfield, Illinois has…" {
		t.Errorf("turn 1 = %+v", g1)
	}
	if len(g1.toolCalls) != 0 {
		t.Errorf("turn 1 toolCalls = %d, want 0", len(g1.toolCalls))
	}
}

// nodeEvent is a gate-internal event (a worker draft, an advisor consult, a
// revision) - tagged with NodeInfo, unlike the orchestrator's own top-level
// events (asstEvent, persistAnswer). Never the user-facing message.
func nodeEvent(path string, parts ...*genai.Part) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "web-researcher"
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	ev.NodeInfo = &session.NodeInfo{Path: path}
	return ev
}

// TestGroupSessionEvents_NodeActivityExcluded guards the leak that made a node's
// internal deliberation (advisor guidance, a worker's raw draft) show up as the
// turn's message: gate-internal events (NodeInfo set) must contribute NEITHER
// asstText NOR toolCalls - only the orchestrator's own top-level events do.
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
	if g.asstText != "The real, vetted answer." {
		t.Errorf("asstText = %q, want only the top-level answer (no node-scoped leak)", g.asstText)
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

// TestGroupSessionEvents_UsageAccumulation covers Turn.usage's data source: the
// orchestrator's own model events carry UsageMetadata, summed per turn - while a
// gate-internal node event's usage (already surfaced separately via DagNodeState)
// must NOT leak into it, mirroring the asstText/toolCalls exclusion above.
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

// TestDeleteChat_ReapsSession pins #352 bug 2: deleting a chat must not
// strand its turns, DAG plan/node state, durable event log, or ADK session -
// all of it lives in tables/services keyed off the chat id, and the "chats"
// row was the only thing DeleteChat used to touch. Also covers a
// GitHub-dispatched chat that predates #512's SessionUser column (id prefix
// github-, no recorded SessionUser): its ADK session was written under
// fallback user "github" - the reap must find it there, not the row (already
// deleted by the time the ADK cleanup runs).
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
	if err := st.SaveTurn(ctx, chatID, "t1"); err != nil {
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

// TestSessionUserForChat_RoundTrip pins the #512 read/write asymmetry fix: a
// GitHub chat created for commenter "alice" resolves "alice" (not the old
// hardcoded "github") via both SessionUserFor and SessionUserForChat, and
// DeleteChat reaps the ADK session under that SAME identity. An
// older/unrecorded GitHub chat (empty SessionUser) still falls back to
// "github"; a non-GitHub chat falls back to "local".
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

// TestGroupSessionEvents_OrchestratorOwnReplyKept guards the regression: the
// orchestrator's own conversational (no-DAG) reply carries NodeInfo (it's
// AgentNode-wrapped too) but must still be captured - only gate-internal
// (different-author) events are excluded.
func TestGroupSessionEvents_OrchestratorOwnReplyKept(t *testing.T) {
	events := []*session.Event{
		userEvent("what is the tallest mountain?"),
		orchestratorAgentNodeEvent("Mount Everest, per National Geographic."),
	}
	groups := groupSessionEvents(slices.Values(events))
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if got := groups[0].asstText; got != "Mount Everest, per National Geographic." {
		t.Errorf("asstText = %q, want the orchestrator's own reply preserved", got)
	}
}
