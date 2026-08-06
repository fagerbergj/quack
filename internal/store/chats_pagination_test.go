package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedChats inserts n chats directly (bypassing CreateChat) with strictly
// increasing UpdatedAt, oldest first, so paging order is deterministic
// regardless of wall-clock resolution. Returns ids oldest-to-newest.
func seedChats(t *testing.T, st *Store, n int) []string {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := "chat-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := st.db.Create(&Chat{ID: id, CreatedAt: ts, UpdatedAt: ts}).Error; err != nil {
			t.Fatalf("seed chat %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	return st
}

// Test case 1: default request with 30 chats stored returns the default page
// size, not 30, and carries a next-page cursor.
func TestListChatsDefaultPageSize(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 30)

	chats, next, err := st.ListChats(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != ChatsPageDefaultLimit {
		t.Fatalf("len(chats) = %d, want default limit %d", len(chats), ChatsPageDefaultLimit)
	}
	if next == "" {
		t.Fatal("next cursor empty, want a next-page signal (30 stored > default limit)")
	}
	// Most-recently-updated first.
	for i := 1; i < len(chats); i++ {
		if chats[i-1].UpdatedAt.Before(chats[i].UpdatedAt) {
			t.Fatalf("chats not ordered most-recently-updated first at index %d", i)
		}
	}
}

// Test case 2: paging through in fixed steps yields every chat exactly once.
func TestListChatsPagingIsExhaustiveAndDedup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ids := seedChats(t, st, 37)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		chats, next, err := st.ListChats(ctx, 10, cursor)
		if err != nil {
			t.Fatalf("ListChats: %v", err)
		}
		for _, c := range chats {
			seen[c.ID]++
		}
		pages++
		if pages > 100 {
			t.Fatal("did not terminate: possible cursor loop")
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != len(ids) {
		t.Fatalf("saw %d distinct chats, want %d", len(seen), len(ids))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("chat %s seen %d times, want exactly 1", id, n)
		}
	}
}

// Test case 3: a chat's updated_at changing mid-page (a run starting between
// two page requests) must not skip or repeat a row - the reason to use a
// keyset cursor instead of an offset.
func TestListChatsCursorStableAcrossConcurrentUpdate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ids := seedChats(t, st, 5) // oldest..newest: ids[0]..ids[4]

	// First page: newest 2 chats (ids[4], ids[3]).
	page1, cursor, err := st.ListChats(ctx, 2, "")
	if err != nil {
		t.Fatalf("ListChats page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("page1 = %v, want [%s %s]", ids2(page1), ids[4], ids[3])
	}
	if cursor == "" {
		t.Fatal("expected a next cursor after page1")
	}

	// A run starts on the oldest chat (ids[0]), bumping it to "now" - past
	// every other row, including ones already returned.
	future := time.Now().Add(time.Hour)
	if err := st.db.Model(&Chat{}).Where("id = ?", ids[0]).Update("updated_at", future).Error; err != nil {
		t.Fatalf("bump ids[0]: %v", err)
	}

	page2, _, err := st.ListChats(ctx, 2, cursor)
	if err != nil {
		t.Fatalf("ListChats page2: %v", err)
	}
	// ids[0] jumped above the cursor boundary captured at page1 and is
	// excluded from page2 (it would reappear at the top of a fresh page1,
	// not retroactively inside an in-flight page walk). The remaining
	// order (ids[2], ids[1]) must come through with no skip or repeat.
	if len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("page2 = %v, want [%s %s] (no skip/repeat despite ids[0]'s update)", ids2(page2), ids[2], ids[1])
	}
}

// Test case 4: a request with no parameters still works.
func TestListChatsNoParams(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 3)

	chats, _, err := st.ListChats(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 3 {
		t.Fatalf("len(chats) = %d, want 3", len(chats))
	}
}

// Test case 5: fewer chats stored than the page size returns them all and
// signals there is no next page.
func TestListChatsFewerThanPageSize(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 3)

	chats, next, err := st.ListChats(ctx, 20, "")
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 3 {
		t.Fatalf("len(chats) = %d, want 3", len(chats))
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty (no more pages)", next)
	}
}

func ids2(chats []Chat) []string {
	out := make([]string, len(chats))
	for i, c := range chats {
		out[i] = c.ID
	}
	return out
}
