package sleeper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockServer replays testdata/get fixtures the same way cmd/qa-mock does,
// so client_test.go never touches the real Sleeper API.
func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := fixtureKey(r.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", "get", key))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	srv := mockServer(t)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestClientState(t *testing.T) {
	c := newTestClient(t)
	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Week != 2 {
		t.Errorf("week = %v, want 2", st.Week)
	}
}

func TestClientLeagueAndRosters(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	league, err := c.League(ctx, testLeague)
	if err != nil {
		t.Fatalf("League: %v", err)
	}
	if league.Name == "" {
		t.Errorf("league name empty")
	}
	rosters, err := c.Rosters(ctx, testLeague)
	if err != nil {
		t.Fatalf("Rosters: %v", err)
	}
	if len(rosters) == 0 {
		t.Errorf("expected rosters, got none")
	}
}

func TestClientLeagueUsersAndMatchups(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	users, err := c.LeagueUsers(ctx, testLeague)
	if err != nil {
		t.Fatalf("LeagueUsers: %v", err)
	}
	if len(users) == 0 {
		t.Errorf("expected league users, got none")
	}
	matchups, err := c.Matchups(ctx, testLeague, 2)
	if err != nil {
		t.Fatalf("Matchups: %v", err)
	}
	if len(matchups) == 0 {
		t.Errorf("expected matchups, got none")
	}
}

func TestClientWeekProjectionsAndStats(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	proj, err := c.WeekProjections(ctx, "2026", 2)
	if err != nil {
		t.Fatalf("WeekProjections: %v", err)
	}
	if len(proj) == 0 {
		t.Errorf("expected projections, got none")
	}
	stats, err := c.WeekStats(ctx, "2026", 2)
	if err != nil {
		t.Fatalf("WeekStats: %v", err)
	}
	if len(stats) == 0 {
		t.Errorf("expected stats, got none")
	}
}

func TestClientLeagueNotFound(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.League(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected an error for a league with no fixture (404)")
	}
}

// TestOkJSONTreatsNullBodyAsNotFound: Sleeper's 200 + literal "null" (an unknown username) is ErrNotFound,
// not a zero-valued struct.
func TestOkJSONTreatsNullBodyAsNotFound(t *testing.T) {
	var zero string
	if _, err := okJSON(&zero, &http.Response{Status: "200 OK"}, []byte("null")); err == nil {
		t.Fatal("expected okJSON to error on a null body")
	}
}

func TestOkJSONPassesThroughRealValue(t *testing.T) {
	v := "hello"
	got, err := okJSON(&v, &http.Response{Status: "200 OK"}, []byte(`"hello"`))
	if err != nil {
		t.Fatalf("okJSON: %v", err)
	}
	if got == nil || *got != "hello" {
		t.Errorf("okJSON(...) = %v, want \"hello\"", got)
	}
}

// TestOkJSONDistinguishesNotFoundFromOtherErrors: only a 404 or null body is ErrNotFound; a 500 is a
// genuine failure callers must not skip.
func TestOkJSONDistinguishesNotFoundFromOtherErrors(t *testing.T) {
	var zero string
	_, err := okJSON[string](nil, &http.Response{Status: "404 Not Found", StatusCode: 404}, []byte("not found"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v, want errors.Is(err, ErrNotFound)", err)
	}
	_, err = okJSON(&zero, &http.Response{Status: "200 OK", StatusCode: 200}, []byte("null"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("null body: err = %v, want errors.Is(err, ErrNotFound)", err)
	}
	_, err = okJSON[string](nil, &http.Response{Status: "500 Internal Server Error", StatusCode: 500}, []byte("boom"))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("500: err = %v, want a non-ErrNotFound error", err)
	}
}

func TestClientCachesRepeatedCalls(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if _, err := c.State(ctx); err != nil {
		t.Fatalf("State: %v", err)
	}
	c.mu.Lock()
	entry, ok := c.cache["state"]
	c.mu.Unlock()
	if !ok {
		t.Fatalf("expected state cached")
	}
	if _, err := c.State(ctx); err != nil {
		t.Fatalf("State (cached): %v", err)
	}
	c.mu.Lock()
	entry2 := c.cache["state"]
	c.mu.Unlock()
	if !entry.expires.Equal(entry2.expires) {
		t.Errorf("expected cache hit to reuse the same entry, expiry changed")
	}
}

func TestResolvePlayer(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if _, err := c.PlayersDump(ctx); err != nil {
		t.Fatalf("PlayersDump: %v", err)
	}
	if ids := c.ResolvePlayer("Buccaneers"); len(ids) == 0 || ids[0] != "TB" {
		t.Errorf("ResolvePlayer(Buccaneers) = %v, want [TB]", ids)
	}
	if ids := c.ResolvePlayer("tb"); len(ids) == 0 || ids[0] != "TB" {
		t.Errorf("ResolvePlayer(tb) = %v, want [TB]", ids)
	}
}

// TestChainStopsOnUnreachable: a broken hop stops the walk and returns the reachable prefix.
func TestChainStopsOnUnreachable(t *testing.T) {
	c := newTestClient(t)
	got := c.Chain(context.Background(), testLeague, 0)
	if len(got) != 2 {
		t.Fatalf("chain = %d leagues, want 2 (testLeague, pastLeague)", len(got))
	}
	if got[0].LeagueId != testLeague || got[1].LeagueId != pastLeague {
		t.Errorf("chain = %v, want [testLeague, pastLeague]", got)
	}
}

// TestChainSeasonsBackCaps proves the limit is honored before any walking.
func TestChainSeasonsBackCaps(t *testing.T) {
	c := newTestClient(t)
	got := c.Chain(context.Background(), testLeague, 1)
	if len(got) != 1 || got[0].LeagueId != testLeague {
		t.Errorf("chain = %v, want just [testLeague]", got)
	}
}

// TestPlayersDumpCoversReferencedIDs: every player_id the roster/matchup/draft/transaction fixtures cite
// must have a dump entry.
func TestPlayersDumpCoversReferencedIDs(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	dump, err := c.PlayersDump(ctx)
	if err != nil {
		t.Fatalf("PlayersDump: %v", err)
	}
	for _, id := range referencedPlayerIDs(t) {
		if _, ok := dump[id]; !ok {
			t.Errorf("player id %q is referenced by a fixture but missing from the players dump", id)
		}
	}
}

// referencedPlayerIDs collects every player_id those fixtures cite for both leagues.
func referencedPlayerIDs(t *testing.T) []string {
	t.Helper()
	ids := map[string]bool{}
	for _, url := range []string{"/v1/league/" + testLeague + "/rosters", "/v1/league/" + pastLeague + "/rosters"} {
		var rosters []struct {
			Players  []string `json:"players"`
			Starters []string `json:"starters"`
		}
		mustUnmarshalFixture(t, url, &rosters)
		for _, r := range rosters {
			addIDs(ids, r.Players)
			addIDs(ids, r.Starters)
		}
	}
	matchupURLs := []string{
		"/v1/league/" + testLeague + "/matchups/1", "/v1/league/" + testLeague + "/matchups/2",
		"/v1/league/" + pastLeague + "/matchups/1", "/v1/league/" + pastLeague + "/matchups/14",
	}
	for _, url := range matchupURLs {
		var matchups []struct {
			Players []string `json:"players"`
		}
		mustUnmarshalFixture(t, url, &matchups)
		for _, m := range matchups {
			addIDs(ids, m.Players)
		}
	}
	for _, url := range []string{"/v1/draft/" + testDraft + "/picks", "/v1/draft/" + pastDraft + "/picks"} {
		var picks []struct {
			PlayerId string `json:"player_id"`
		}
		mustUnmarshalFixture(t, url, &picks)
		for _, p := range picks {
			ids[p.PlayerId] = true
		}
	}
	txURLs := []string{
		"/v1/league/" + testLeague + "/transactions/1", "/v1/league/" + testLeague + "/transactions/2",
		"/v1/league/" + pastLeague + "/transactions/1", "/v1/league/" + pastLeague + "/transactions/5",
	}
	for _, url := range txURLs {
		var txs []struct {
			Adds  map[string]int `json:"adds"`
			Drops map[string]int `json:"drops"`
		}
		mustUnmarshalFixture(t, url, &txs)
		for _, tx := range txs {
			addKeys(ids, tx.Adds)
			addKeys(ids, tx.Drops)
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out
}

func addIDs(ids map[string]bool, list []string) {
	for _, id := range list {
		ids[id] = true
	}
}

func addKeys(ids map[string]bool, m map[string]int) {
	for id := range m {
		ids[id] = true
	}
}

func mustUnmarshalFixture(t *testing.T, url string, dst any) {
	t.Helper()
	data := readFixture(t, url)
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("unmarshal fixture for %s: %v", url, err)
	}
}

func TestCachedDropsExpiredEntriesOnInsert(t *testing.T) {
	c := newTestClient(t)
	c.mu.Lock()
	c.cache["stale"] = cacheEntry{value: struct{}{}, expires: time.Now().Add(-time.Minute)}
	c.mu.Unlock()
	if _, err := c.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	_, stale := c.cache["stale"]
	c.mu.Unlock()
	if stale {
		t.Error("expired entry survived a cache insert")
	}
}
