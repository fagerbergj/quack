package store

import (
	"context"
	"errors"
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

// seedChatsScoped is seedChats plus an explicit archived flag per row, letting
// a fixture interleave archived and active rows in updated_at order - the
// shape that filtering archived out of an already-fetched page corrupts.
// Returns ids oldest-to-newest.
func seedChatsScoped(t *testing.T, st *Store, archived []bool) []string {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, len(archived))
	for i, a := range archived {
		id := "chat-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := st.db.Create(&Chat{ID: id, CreatedAt: ts, UpdatedAt: ts, Archived: a}).Error; err != nil {
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
// size, not 30, and carries a next-page token.
func TestListChatsDefaultPageSize(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 30)

	chats, next, err := st.ListChats(ctx, 0, "", ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != ChatsPageDefaultLimit {
		t.Fatalf("len(chats) = %d, want default limit %d", len(chats), ChatsPageDefaultLimit)
	}
	if next == "" {
		t.Fatal("next page token empty, want a next-page signal (30 stored > default limit)")
	}
	// Most-recently-updated first.
	for i := 1; i < len(chats); i++ {
		if chats[i-1].UpdatedAt.Before(chats[i].UpdatedAt) {
			t.Fatalf("chats not ordered most-recently-updated first at index %d", i)
		}
	}
}

// Test case 2: paging through in fixed steps yields every chat exactly once.
// The token is round-tripped as an opaque string - never decoded or
// inspected by the caller - proving pagination doesn't secretly depend on
// the caller understanding it.
func TestListChatsPagingIsExhaustiveAndDedup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ids := seedChats(t, st, 37)

	seen := map[string]int{}
	token := ""
	pages := 0
	for {
		chats, next, err := st.ListChats(ctx, 10, token, ChatsScopeActive)
		if err != nil {
			t.Fatalf("ListChats: %v", err)
		}
		for _, c := range chats {
			seen[c.ID]++
		}
		pages++
		if pages > 100 {
			t.Fatal("did not terminate: possible page-token loop")
		}
		if next == "" {
			break
		}
		token = next
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
// keyset token instead of an offset.
func TestListChatsTokenStableAcrossConcurrentUpdate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ids := seedChats(t, st, 5) // oldest..newest: ids[0]..ids[4]

	// First page: newest 2 chats (ids[4], ids[3]).
	page1, token, err := st.ListChats(ctx, 2, "", ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("page1 = %v, want [%s %s]", ids2(page1), ids[4], ids[3])
	}
	if token == "" {
		t.Fatal("expected a next page token after page1")
	}

	// A run starts on the oldest chat (ids[0]), bumping it to "now" - past
	// every other row, including ones already returned.
	future := time.Now().Add(time.Hour)
	if err := st.db.Model(&Chat{}).Where("id = ?", ids[0]).Update("updated_at", future).Error; err != nil {
		t.Fatalf("bump ids[0]: %v", err)
	}

	page2, _, err := st.ListChats(ctx, 2, token, ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats page2: %v", err)
	}
	// ids[0] jumped above the token's boundary captured at page1 and is
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

	chats, _, err := st.ListChats(ctx, 0, "", ChatsScopeActive)
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

	chats, next, err := st.ListChats(ctx, 20, "", ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 3 {
		t.Fatalf("len(chats) = %d, want 3", len(chats))
	}
	if next != "" {
		t.Fatalf("next page token = %q, want empty (no more pages)", next)
	}
}

// TestListChatsTokenIssuedForWrongSortRejected pins the contract: a token
// carries the ordering it was issued under, and replaying it against a
// different one is an error, never silently honored. chatsSort has exactly
// one value today, so this is exercised by hand-forging a token under a
// different (hypothetical) sort - the shape a future second ordering would
// produce.
func TestListChatsTokenIssuedForWrongSortRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 5)

	wrongSort := encodeChatsPageToken(chatsPageToken{Sort: "title_asc", ID: "chat-aa", UpdatedAt: time.Now()})
	if _, _, err := st.ListChats(ctx, 0, wrongSort, ChatsScopeActive); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("ListChats with a token issued for a different sort: err = %v, want ErrInvalidPageToken", err)
	}
}

// TestListChatsMalformedTokenRejected: garbage in the page_token param is a
// client error (ErrInvalidPageToken), not a panic or a silent first page.
func TestListChatsMalformedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChats(t, st, 3)

	if _, _, err := st.ListChats(ctx, 0, "not-a-valid-token!!", ChatsScopeActive); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("ListChats with a malformed token: err = %v, want ErrInvalidPageToken", err)
	}
}

// #809 test case 1: with archived chats present, a default-scope page of
// limit N returns N active chats, not N-minus-however-many-were-archived.
func TestListChatsScopeActiveReturnsFullPageDespiteArchived(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// 5 active, 3 archived, interleaved - a page of 3 must still be 3 active rows.
	seedChatsScoped(t, st, []bool{false, true, false, true, false, true, false, false})

	chats, next, err := st.ListChats(ctx, 3, "", ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 3 {
		t.Fatalf("len(chats) = %d, want 3 (a full page of active rows, not fewer)", len(chats))
	}
	for _, c := range chats {
		if c.Archived {
			t.Fatalf("archived chat %s present in an active-scope page", c.ID)
		}
	}
	if next == "" {
		t.Fatal("next page token empty, want a next-page signal (5 active > page size 3)")
	}
}

// #809 test case 2 (the one that matters): archived and active rows interleave
// in updated_at order - a fixture with archived rows clustered at one end
// would pass even with the old bug, since it never had to skip past a
// discarded archived row mid-page. Paging the active scope to exhaustion must
// see every active chat exactly once and no archived chat at all.
func TestListChatsScopeActiveNeverSkipsOrRepeatsAcrossInterleavedArchived(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pattern := []bool{false, true, true, false, true, false, false, true, false, true, false, false, true}
	ids := seedChatsScoped(t, st, pattern)

	wantActive := map[string]bool{}
	for i, a := range pattern {
		if !a {
			wantActive[ids[i]] = true
		}
	}

	seen := map[string]int{}
	token := ""
	pages := 0
	for {
		chats, next, err := st.ListChats(ctx, 3, token, ChatsScopeActive)
		if err != nil {
			t.Fatalf("ListChats: %v", err)
		}
		for _, c := range chats {
			if c.Archived {
				t.Fatalf("archived chat %s returned from an active-scope page", c.ID)
			}
			seen[c.ID]++
		}
		pages++
		if pages > 100 {
			t.Fatal("did not terminate: possible page-token loop")
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(seen) != len(wantActive) {
		t.Fatalf("saw %d distinct active chats, want %d", len(seen), len(wantActive))
	}
	for id, n := range seen {
		if !wantActive[id] {
			t.Fatalf("chat %s is not active but was returned", id)
		}
		if n != 1 {
			t.Fatalf("chat %s seen %d times, want exactly 1", id, n)
		}
	}
}

// #809 test case 3: an archived-scope request returns only archived chats and
// pages independently of the active cursor (a fresh token walk, not sharing
// position with an active-scope walk over the same interleaved fixture).
func TestListChatsScopeArchivedPagesIndependently(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pattern := []bool{false, true, true, false, true, false, true}
	ids := seedChatsScoped(t, st, pattern)

	wantArchived := map[string]bool{}
	for i, a := range pattern {
		if a {
			wantArchived[ids[i]] = true
		}
	}

	seen := map[string]int{}
	token := ""
	for {
		chats, next, err := st.ListChats(ctx, 2, token, ChatsScopeArchived)
		if err != nil {
			t.Fatalf("ListChats: %v", err)
		}
		for _, c := range chats {
			if !c.Archived {
				t.Fatalf("active chat %s returned from an archived-scope page", c.ID)
			}
			seen[c.ID]++
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(seen) != len(wantArchived) {
		t.Fatalf("saw %d distinct archived chats, want %d", len(seen), len(wantArchived))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("chat %s seen %d times, want exactly 1", id, n)
		}
	}
}

// #809 test case 4: a token issued for one scope, replayed against another,
// is rejected (ErrInvalidPageToken) rather than silently producing a page
// that mixes rows from both scopes.
func TestListChatsScopeMismatchRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChatsScoped(t, st, []bool{false, true, false, true, false})

	_, activeToken, err := st.ListChats(ctx, 1, "", ChatsScopeActive)
	if err != nil {
		t.Fatalf("ListChats (active): %v", err)
	}
	if activeToken == "" {
		t.Fatal("expected a next page token for the active scope")
	}

	if _, _, err := st.ListChats(ctx, 1, activeToken, ChatsScopeArchived); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("ListChats replaying an active-scope token against archived scope: err = %v, want ErrInvalidPageToken", err)
	}
	if _, _, err := st.ListChats(ctx, 1, activeToken, ChatsScopeAll); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("ListChats replaying an active-scope token against all scope: err = %v, want ErrInvalidPageToken", err)
	}
}

// TestListChatsTokenWithoutScopeDefaultsToActive pins the old-token decision:
// a token minted before scoping existed (no Scope field) is treated as
// ChatsScopeActive - the pre-#809 default - rather than rejected outright, so
// a cursor a client is already holding does not 500 on the next release.
func TestListChatsTokenWithoutScopeDefaultsToActive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedChatsScoped(t, st, []bool{false, true, false})

	legacyToken := encodeChatsPageToken(chatsPageToken{Sort: chatsSortUpdatedAtDesc, ID: "chat-aa", UpdatedAt: time.Now()})

	if _, _, err := st.ListChats(ctx, 1, legacyToken, ChatsScopeActive); err != nil {
		t.Fatalf("legacy (scopeless) token against active scope: %v, want success", err)
	}
	if _, _, err := st.ListChats(ctx, 1, legacyToken, ChatsScopeArchived); !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("legacy (scopeless) token against archived scope: err = %v, want ErrInvalidPageToken", err)
	}
}

func ids2(chats []Chat) []string {
	out := make([]string, len(chats))
	for i, c := range chats {
		out[i] = c.ID
	}
	return out
}
