package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestConversationSessionsFiltersReadsNotWrites: the view's Events() yields
// only user/orchestrator events, while AppendEvent (unwrapping the view)
// persists EVERY author to the underlying service untouched.
func TestConversationSessionsFiltersReadsNotWrites(t *testing.T) {
	raw := session.InMemoryService()
	view := conversationSessions{raw}
	ctx := context.Background()

	created, err := view.Create(ctx, &session.CreateRequest{AppName: "quack", UserID: "u", SessionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	appendAs := func(author, text string) {
		ev := session.NewEvent(ctx, "inv")
		ev.Author = author
		ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}
		if author == "user" {
			ev.Content.Role = "user"
		}
		// Append THROUGH the view (as the runner would): must unwrap and land.
		if err := view.AppendEvent(ctx, created.Session, ev); err != nil {
			t.Fatalf("AppendEvent(%s): %v", author, err)
		}
	}
	appendAs("user", "hello")
	appendAs("orchestrator", "hi there")
	appendAs("code-implementer", "WORKER-INTERNALS")
	appendAs("quack-gate", "PROMPT-INTERNALS")

	// The view's read: conversation only.
	got, err := view.Get(ctx, &session.GetRequest{AppName: "quack", UserID: "u", SessionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	evs := got.Session.Events()
	if evs.Len() != 2 {
		t.Fatalf("view Len = %d, want 2 (user + orchestrator only)", evs.Len())
	}
	for ev := range evs.All() {
		if !isConversationEvent(ev) {
			t.Errorf("non-conversation event leaked through the view: author=%q", ev.Author)
		}
	}

	// The raw service still holds everything (writes were never filtered).
	rawGot, err := raw.Get(ctx, &session.GetRequest{AppName: "quack", UserID: "u", SessionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if n := rawGot.Session.Events().Len(); n != 4 {
		t.Fatalf("raw Len = %d, want 4 (all authors persisted)", n)
	}
}
