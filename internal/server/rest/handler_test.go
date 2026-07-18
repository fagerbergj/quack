package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// TestSessionUser: the ADK session identity derives from the chat id shape —
// github-<owner>-<repo>-<number> (store.SetChatGitHub) resolves to the
// webhook's runUserID ("github", internal/github.extension.go), any other id
// to the first-party local user.
func TestSessionUser(t *testing.T) {
	cases := map[string]string{
		"github-acme-widget-app-7": githubSessionUser,
		"abc123":                   userID,
		"github":                   userID, // no trailing "-": not the dispatched shape
	}
	for chatID, want := range cases {
		if got := sessionUser(chatID); got != want {
			t.Errorf("sessionUser(%q) = %q, want %q", chatID, got, want)
		}
	}
}

// TestGetChat_GithubSessionUser pins the bug in #352: a GitHub-dispatched
// chat's turns are written to its ADK session under user "github" (the
// webhook's runUserID), not "local". GetChat must resolve turns under the
// SAME user the webhook wrote them under, or the chat renders with no
// content even though the run completed and the events exist.
func TestGetChat_GithubSessionUser(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	const chatID = "github-acme-widget-app-7"
	if err := h.store.SetChatGitHub(ctx, chatID, "acme/widget-app", "https://github.com/acme/widget-app/pull/7"); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	if err := h.store.SaveTurn(ctx, chatID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}

	resp, err := h.store.Sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: githubSessionUser, SessionID: chatID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	userEv := session.NewEvent(ctx, "test")
	userEv.Author = "user"
	userEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review this PR"}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, userEv); err != nil {
		t.Fatalf("AppendEvent user: %v", err)
	}
	asstEv := session.NewEvent(ctx, "test")
	asstEv.Author = "orchestrator"
	asstEv.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Looks good."}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, asstEv); err != nil {
		t.Fatalf("AppendEvent asst: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID, nil)
	rec := httptest.NewRecorder()
	h.GetChat(rec, req, chatID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail schema.ChatDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(detail.Turns) != 1 {
		t.Fatalf("turns = %d, want 1 (content must resolve under the github session user, not local)", len(detail.Turns))
	}
	if len(detail.Turns[0].Output) == 0 {
		t.Fatal("turn output is empty, want the assistant's message item")
	}
	msg, err := detail.Turns[0].Output[0].AsMessageOutputItem()
	if err != nil {
		t.Fatalf("AsMessageOutputItem: %v", err)
	}
	if len(msg.Content) == 0 {
		t.Fatal("message content is empty")
	}
	part, err := msg.Content[0].AsOutputTextPart()
	if err != nil {
		t.Fatalf("AsOutputTextPart: %v", err)
	}
	if part.Text != "Looks good." {
		t.Errorf("answer text = %q, want %q", part.Text, "Looks good.")
	}
}

// TestChatStatusIdle: a fresh chat with no turns, no session activity, and no
// live run is idle.
func TestChatStatusIdle(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusIdle {
		t.Errorf("status = %q, want idle", got.Status)
	}
	if got.PendingQuestion != nil {
		t.Errorf("pending_question = %v, want nil", got.PendingQuestion)
	}
}

// TestToSummaryGithubFields: toSummary surfaces the persisted GitHub link
// fields, and leaves them nil for a chat that never had them set.
func TestToSummaryGithubFields(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	id := "github-acme-widget-app-7"
	if err := h.store.SetChatGitHub(ctx, id, "acme/widget-app", "https://github.com/acme/widget-app/pull/7"); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	c, err := h.store.GetChat(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %+v err=%v", c, err)
	}
	got := h.toSummary(ctx, *c)
	if got.GithubUrl == nil || *got.GithubUrl != "https://github.com/acme/widget-app/pull/7" {
		t.Errorf("github_url = %v, want the pull URL", got.GithubUrl)
	}
	if got.GithubRepo == nil || *got.GithubRepo != "acme/widget-app" {
		t.Errorf("github_repo = %v, want acme/widget-app", got.GithubRepo)
	}

	plain, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	got = h.toSummary(ctx, *plain)
	if got.GithubUrl != nil || got.GithubRepo != nil {
		t.Errorf("non-github chat should have nil github fields, got url=%v repo=%v", got.GithubUrl, got.GithubRepo)
	}
}

// TestChatStatusRunning: the hub having a live topic for the chat wins over
// everything else (checked first).
func TestChatStatusRunning(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	h.hub.Publish(c.ID, 1, stream.SSEEvent{Name: "node_start"})

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

// TestChatStatusNeedsInput: a pending get_user_choice clarification in the
// chat's session — the SAME scan Run's resume dispatch uses
// (orchestrator.LatestPendingQuestion) — surfaces as needs_input with the
// question text.
func TestChatStatusNeedsInput(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	const callID = "call-1"
	resp, err := h.store.Sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	ask := session.NewEvent(ctx, "test")
	ask.Author = "orchestrator"
	ask.LongRunningToolIDs = []string{callID}
	ask.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{ID: callID, Name: tools.ChoiceToolName, Args: map[string]any{
			"question": "which Springfield?", "options": []string{"Illinois", "Missouri"},
		}},
	}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, ask); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	placeholder := session.NewEvent(ctx, "test")
	placeholder.Author = "user"
	placeholder.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{"status": "pending"}},
	}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, placeholder); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusNeedsInput {
		t.Fatalf("status = %q, want needs_input", got.Status)
	}
	if got.PendingQuestion == nil || *got.PendingQuestion != "which Springfield?" {
		t.Errorf("pending_question = %v, want %q", got.PendingQuestion, "which Springfield?")
	}
}

// TestChatStatusFailed: the last turn's DAG has a failed node and no assistant
// text followed it.
func TestChatStatusFailed(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := h.store.SaveDagPlan(ctx, c.ID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: "n1", PlanID: "p1", Status: "failed", Error: "boom"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// TestBuildTurnUsage covers PR2 item 2: buildTurn must populate Turn.usage from
// the orchestrator's own accumulated token counts (store.TurnContent, itself
// summed from stored ADK session events — see store.groupSessionEvents).
// input_tokens = prompt; output_tokens folds candidates + reasoning together
// (schema.Usage has no separate reasoning field).
func TestBuildTurnUsage(t *testing.T) {
	tc := store.TurnContent{
		ID:               "t1",
		CreatedAt:        time.Now(),
		UserText:         "what is the tallest mountain?",
		AsstText:         "Mount Everest.",
		PromptTokens:     40,
		CompletionTokens: 15,
		ReasoningTokens:  2,
		Model:            "gpt-oss-120b",
	}

	turn := buildTurn(tc)

	if turn.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	if turn.Usage.InputTokens == nil || *turn.Usage.InputTokens != 40 {
		t.Errorf("InputTokens = %v, want 40", turn.Usage.InputTokens)
	}
	if turn.Usage.OutputTokens == nil || *turn.Usage.OutputTokens != 17 {
		t.Errorf("OutputTokens = %v, want 17 (completion + reasoning)", turn.Usage.OutputTokens)
	}
	if turn.Model == nil || *turn.Model != "gpt-oss-120b" {
		t.Errorf("Model = %v, want gpt-oss-120b (from the persisted turn row)", turn.Model)
	}
}

// TestBuildTurnUsageNilWhenAbsent covers a DAG-only turn: the orchestrator itself
// recorded no tokens (all the work happened in gated nodes, surfaced separately
// via DagNodeState) — Turn.usage must stay nil, not a zero-valued struct, so the
// frontend can tell "no data" from "genuinely zero usage".
func TestBuildTurnUsageNilWhenAbsent(t *testing.T) {
	tc := store.TurnContent{ID: "t2", CreatedAt: time.Now(), UserText: "research X", AsstText: "The vetted answer."}

	turn := buildTurn(tc)

	if turn.Usage != nil {
		t.Errorf("Usage = %+v, want nil", turn.Usage)
	}
	if turn.Model != nil {
		t.Errorf("Model = %v, want nil (DAG turns carry no orchestrator model)", turn.Model)
	}
}
