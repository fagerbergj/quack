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
	if chats, err := st.ListChats(ctx); err != nil || len(chats) != 1 {
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
// just like a gate-internal node's — "author, not NodeInfo" is what distinguishes
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
// revision) — tagged with NodeInfo, unlike the orchestrator's own top-level
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
// asstText NOR toolCalls — only the orchestrator's own top-level events do.
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
// orchestrator's own model events carry UsageMetadata, summed per turn — while a
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

// TestGroupSessionEvents_OrchestratorOwnReplyKept guards the regression: the
// orchestrator's own conversational (no-DAG) reply carries NodeInfo (it's
// AgentNode-wrapped too) but must still be captured — only gate-internal
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
