package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/schema"
)

// commitRoleFact seeds a role:coding memory with a chat_id provenance stamp -
// exactly the pre-#1262 shape (RepoKey="" made every worker write here).
func commitRoleFact(t *testing.T, s *memory.Store, chatID, content string) {
	t.Helper()
	if _, err := s.Commit(context.Background(), memory.Scope{Role: memory.RoleCoding}, "test",
		memory.Provenance{ChatID: chatID}, []memory.Candidate{{Content: content, Metadata: map[string]string{"bucket": "role"}}}, ""); err != nil {
		t.Fatalf("Commit(%q): %v", content, err)
	}
}

// TestRescopeMemories_DryRunThenApply seeds a role:coding memory whose chat
// has a GitHub origin (owner/repo) alongside one with no origin, dry-runs the
// rescope (tallies but writes nothing), then applies it and confirms the
// point actually moved to repo:github.com/acme/games and the untethered one
// stayed in role:coding.
func TestRescopeMemories_DryRunThenApply(t *testing.T) {
	ctx := context.Background()
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)

	ghChat, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	origin, _ := json.Marshal(extsdk.ChatOrigin{
		Extension: "github", Label: "acme/games#1",
		Labels: map[string][]extsdk.LabelValue{"repo": {{Value: "acme/games"}}},
	})
	if err := h.store.SetChatOrigin(ctx, ghChat.ID, "u1", string(origin)); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	plainChat, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	commitRoleFact(t, h.taskMem, ghChat.ID, "needs minSdk 30")
	commitRoleFact(t, h.taskMem, plainChat.ID, "unrelated note")

	dryReq := httptest.NewRequest(http.MethodPost, "/api/v1/memories/rescope", strings.NewReader(`{"apply":false}`))
	dryW := httptest.NewRecorder()
	h.RescopeMemories(dryW, dryReq)
	if dryW.Code != http.StatusOK {
		t.Fatalf("dry run status = %d, want 200", dryW.Code)
	}
	var dryReport schema.RescopeReport
	if err := json.NewDecoder(dryW.Body).Decode(&dryReport); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dryReport.Applied {
		t.Fatalf("dry run report.Applied = true, want false")
	}
	if len(dryReport.Repos) != 1 || dryReport.Repos[0].Repo != "github.com/acme/games" || dryReport.Repos[0].Count != 1 {
		t.Fatalf("dry run repos = %+v, want one github.com/acme/games with count 1", dryReport.Repos)
	}

	// Dry run must not have written anything - still all in role:coding.
	mems, _, err := h.taskMem.List(ctx, []string{"role:coding"}, 0, 0, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("role:coding after dry run has %d memories, want 2 (nothing moved yet)", len(mems))
	}

	applyReq := httptest.NewRequest(http.MethodPost, "/api/v1/memories/rescope", strings.NewReader(`{"apply":true}`))
	applyW := httptest.NewRecorder()
	h.RescopeMemories(applyW, applyReq)
	if applyW.Code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200", applyW.Code)
	}
	var applyReport schema.RescopeReport
	if err := json.NewDecoder(applyW.Body).Decode(&applyReport); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !applyReport.Applied {
		t.Fatalf("apply report.Applied = false, want true")
	}

	repoMems, _, err := h.taskMem.List(ctx, []string{"repo:github.com/acme/games"}, 0, 0, false)
	if err != nil {
		t.Fatalf("List repo bucket: %v", err)
	}
	if len(repoMems) != 1 || repoMems[0].Content != "needs minSdk 30" {
		t.Fatalf("repo:github.com/acme/games = %+v, want the one GitHub-origin memory", repoMems)
	}
	roleMems, _, err := h.taskMem.List(ctx, []string{"role:coding"}, 0, 0, false)
	if err != nil {
		t.Fatalf("List role bucket: %v", err)
	}
	if len(roleMems) != 1 || roleMems[0].Content != "unrelated note" {
		t.Fatalf("role:coding after apply = %+v, want only the no-origin memory left", roleMems)
	}
}
