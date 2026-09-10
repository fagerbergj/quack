package memory

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestFirstStep(t *testing.T) {
	user := func(text string) *genai.Content {
		return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}}
	}
	modelCall := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "calling web_search"}}}
	// Function responses are role=user but textless - must not count as the user turn.
	funcResp := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "web_search"}}}}
	const q = "research native plants"

	tests := []struct {
		name     string
		contents []*genai.Content
		want     bool
	}{
		{"step 1: only user message", []*genai.Content{user(q)}, true},
		{"step 2: model produced output", []*genai.Content{user(q), modelCall}, false},
		{"step 3: model + func response", []*genai.Content{user(q), modelCall, funcResp}, false},
		// Long-lived session (orchestrator): prior turn then a fresh user message.
		{"new turn after prior turn", []*genai.Content{user("old question"), modelCall, user(q)}, true},
		{"new turn, then model output", []*genai.Content{user("old question"), modelCall, user(q), modelCall}, false},
		// A trailing func response (textless, role=user) means the model already acted.
		{"func response after user is not first", []*genai.Content{user(q), funcResp}, false},
		{"empty contents", nil, true},
	}
	for _, tt := range tests {
		if got := firstStep(tt.contents); got != tt.want {
			t.Errorf("%s: firstStep = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Recall is the gate-side preload twin for external workers: formatted block
// on a hit, "" on empty/nil - and a nil store must be safe (memory disabled).
func TestStoreRecall(t *testing.T) {
	consolidator := fakeModel{reply: `{"ops":[{"action":"ADD","content":"build with make dev, not npm run build","kind":"convention"}]}`}
	s := newSQLiteStore(t, "task", consolidator)
	sc := Scope{Role: RoleCoding}
	if _, err := s.Commit(context.Background(), sc, "explorer", Provenance{}, []Candidate{{Content: "build with make dev"}}, "report"); err != nil {
		t.Fatal(err)
	}
	got := s.Recall(context.Background(), sc, "how do I build this repo")
	if !strings.Contains(got, "<MEMORY>") || !strings.Contains(got, "make dev") {
		t.Fatalf("recall block wrong: %q", got)
	}
	if got := s.Recall(context.Background(), Scope{Role: "other"}, "anything"); got != "" {
		t.Fatalf("foreign-scope recall must be empty, got %q", got)
	}
	var nilStore *Store
	if got := nilStore.Recall(context.Background(), sc, "q"); got != "" {
		t.Fatal("nil store must recall nothing")
	}
}

// TestRecallWithHits_PopulatesScore covers the #1257 review finding: a Delivered hit
// must carry the real cosine score, not the zero value every memory.recall ledger
// entry silently recorded before recall() threaded its scored points back out.
func TestRecallWithHits_PopulatesScore(t *testing.T) {
	consolidator := fakeModel{reply: `{"ops":[{"action":"ADD","content":"build with make dev, not npm run build","kind":"convention"}]}`}
	s := newSQLiteStore(t, "task", consolidator)
	sc := Scope{Role: RoleCoding}
	if _, err := s.Commit(context.Background(), sc, "explorer", Provenance{}, []Candidate{{Content: "build with make dev"}}, "report"); err != nil {
		t.Fatal(err)
	}
	text, hits := s.RecallWithHits(context.Background(), sc, "how do I build this repo")
	if text == "" {
		t.Fatal("expected a non-empty recall block")
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly 1", hits)
	}
	// fakeEmbedder returns the same fixed vector for every text, so cosine
	// similarity is exactly 1 - any non-zero value proves the score reached
	// Delivered rather than being silently dropped.
	if hits[0].Score == 0 {
		t.Fatalf("hits[0].Score = %v, want non-zero (real cosine similarity)", hits[0].Score)
	}
}

// TestCapForInjection_TruncatesAndReports covers epic #1255 P2's byte-budget
// requirement: once cumulative Content bytes exceed budget, later hits are
// dropped and truncated is reported - not silently swallowed.
func TestCapForInjection_TruncatesAndReports(t *testing.T) {
	hits := []Delivered{{ID: "a", Content: strings.Repeat("x", 6)}, {ID: "b", Content: strings.Repeat("y", 6)}}
	kept, truncated := CapForInjection(hits, 10)
	if !truncated {
		t.Fatal("truncated = false, want true (12 bytes over a 10-byte budget)")
	}
	if len(kept) != 1 || kept[0].ID != "a" {
		t.Fatalf("kept = %+v, want just the first hit", kept)
	}
	kept, truncated = CapForInjection(hits, 100)
	if truncated || len(kept) != 2 {
		t.Fatalf("kept = %+v truncated = %v, want both hits under budget", kept, truncated)
	}
	// budget<=0 disables the cap entirely.
	kept, truncated = CapForInjection(hits, 0)
	if truncated || len(kept) != 2 {
		t.Fatalf("budget<=0: kept = %+v truncated = %v, want the cap disabled", kept, truncated)
	}
}

// TestRecallForTool_CapsToTopKAndScopeIsolation covers two of P2's required
// verifications together: k narrows but never widens past the store's own
// top_k, and a caller in one repo's scope never sees another repo's memories.
func TestRecallForTool_CapsToTopKAndScopeIsolation(t *testing.T) {
	consolidator := fakeModel{reply: `{"ops":[{"action":"ADD","content":"seed","kind":"note"}]}`}
	s := newSQLiteStore(t, "task", consolidator) // top_k=5, min_score=0.5
	sc := Scope{Repo: "acme/repo-a"}
	other := Scope{Repo: "acme/repo-b"}
	for i := 0; i < 3; i++ {
		if _, err := s.Commit(context.Background(), sc, "explorer", Provenance{}, []Candidate{{Content: "repo-a fact"}}, ""); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if _, err := s.Commit(context.Background(), other, "explorer", Provenance{}, []Candidate{{Content: "repo-b secret"}}, ""); err != nil {
		t.Fatalf("commit other repo: %v", err)
	}

	// k=1 narrows below top_k.
	hits, _ := s.RecallForTool(context.Background(), sc, "fact", 1)
	if len(hits) != 1 {
		t.Fatalf("k=1: got %d hits, want 1", len(hits))
	}
	// k > top_k never widens past top_k.
	hits, _ = s.RecallForTool(context.Background(), sc, "fact", 999)
	if len(hits) > s.TopK() {
		t.Fatalf("k=999: got %d hits, want capped at top_k=%d", len(hits), s.TopK())
	}
	for _, h := range hits {
		if strings.Contains(h.Content, "repo-b") {
			t.Fatalf("scope isolation violated: repo-a's recall returned repo-b's memory: %+v", h)
		}
	}
	// A caller scoped to repo-b never sees repo-a's memories either.
	hits, _ = s.RecallForTool(context.Background(), other, "fact", 0)
	for _, h := range hits {
		if strings.Contains(h.Content, "repo-a") {
			t.Fatalf("scope isolation violated: repo-b's recall returned repo-a's memory: %+v", h)
		}
	}
}
