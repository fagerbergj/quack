package sleeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// Per-endpoint TTLs: state moves fastest; players/projections/stats are bulk fetches worth holding longer.
const (
	ttlState       = 5 * time.Minute
	ttlLeague      = 10 * time.Minute
	ttlMatchups    = 2 * time.Minute
	ttlPlayersDump = 24 * time.Hour
	ttlStatMap     = time.Hour

	// ttlKickoff: kickoff/venue move only on a flex decision announced days ahead; lock state is per call.
	ttlKickoff = 6 * time.Hour

	// ttlLive is for endpoints wanted fresh during a live event (waivers, a live draft, trending).
	ttlLive = 2 * time.Minute

	// maxChainSeasons bounds Chain against a cyclic previous_league_id chain; no real league runs this deep.
	maxChainSeasons = 10
)

// Client is an in-memory-cached wrapper over the generated Sleeper client.
type Client struct {
	gen *sleepergen.ClientWithResponses

	mu    sync.Mutex
	cache map[string]cacheEntry

	namesMu sync.RWMutex
	names   map[string][]string // lowercased "first last"/last name/DEF team code -> player_ids
}

type cacheEntry struct {
	value   any
	expires time.Time
}

// NewClient builds a Client against baseURL (production, or qa-mock in tests); a nil httpClient uses the
// generated client's default.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	var opts []sleepergen.ClientOption
	if httpClient != nil {
		opts = append(opts, sleepergen.WithHTTPClient(httpClient))
	}
	gen, err := sleepergen.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("sleeper: new client: %w", err)
	}
	return &Client{gen: gen, cache: map[string]cacheEntry{}}, nil
}

// cached replays an unexpired value under key, else fetches and stores it, sweeping expired entries
// so per-week and per-player keys don't accumulate.
func cached[T any](c *Client, key string, ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.value.(T), nil
	}
	c.mu.Unlock()

	v, err := fetch()
	if err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	now := time.Now()
	// ponytail: O(n) sweep per insert, fine for hundreds of keys; add an LRU if the key count grows.
	maps.DeleteFunc(c.cache, func(_ string, e cacheEntry) bool { return now.After(e.expires) })
	c.cache[key] = cacheEntry{value: v, expires: now.Add(ttl)}
	c.mu.Unlock()
	return v, nil
}

func (c *Client) State(ctx context.Context) (*sleepergen.NflState, error) {
	return cached(c, "state", ttlState, func() (*sleepergen.NflState, error) {
		resp, err := c.gen.GetNflStateWithResponse(ctx)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get state: %w", err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

//nolint:dupl // each single-value cached lookup differs only by type and endpoint
func (c *Client) League(ctx context.Context, leagueID string) (*sleepergen.League, error) {
	return cached(c, "league:"+leagueID, ttlLeague, func() (*sleepergen.League, error) {
		resp, err := c.gen.GetLeagueWithResponse(ctx, leagueID)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get league %s: %w", leagueID, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

// cachedList is cached for generated calls returning a *slice-or-map JSON200 plus the raw response.
func cachedList[T any](c *Client, key string, ttl time.Duration, fetch func() (*T, *http.Response, []byte, error)) (T, error) {
	return cached(c, key, ttl, func() (T, error) {
		var zero T
		json, resp, body, err := fetch() //nolint:bodyclose // ClientWithResponses already read and closed the body
		if err != nil {
			return zero, err
		}
		out, err := okJSON(json, resp, body)
		if err != nil {
			return zero, err
		}
		return *out, nil
	})
}

//nolint:dupl // each generated ...WithResponse call differs only by type and method name
func (c *Client) Rosters(ctx context.Context, leagueID string) ([]sleepergen.Roster, error) {
	return cachedList(c, "rosters:"+leagueID, ttlLeague, func() (*[]sleepergen.Roster, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueRostersWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get rosters %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) LeagueUsers(ctx context.Context, leagueID string) ([]sleepergen.LeagueUser, error) {
	return cachedList(c, "league_users:"+leagueID, ttlLeague, func() (*[]sleepergen.LeagueUser, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueUsersWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get league users %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) Matchups(ctx context.Context, leagueID string, week int) ([]sleepergen.Matchup, error) {
	key := fmt.Sprintf("matchups:%s:%d", leagueID, week)
	return cachedList(c, key, ttlMatchups, func() (*[]sleepergen.Matchup, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueMatchupsWithResponse(ctx, leagueID, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get matchups %s week %d: %w", leagueID, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// WeekProjections and WeekStats share Sleeper's stat-map shape and hour TTL.
//
//nolint:dupl // see Rosters
func (c *Client) WeekProjections(ctx context.Context, season string, week int) (map[string]sleepergen.StatMap, error) {
	key := fmt.Sprintf("projections:%s:%d", season, week)
	return cachedList(c, key, ttlStatMap, func() (*map[string]sleepergen.StatMap, *http.Response, []byte, error) {
		resp, err := c.gen.GetWeekProjectionsWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get projections %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) WeekStats(ctx context.Context, season string, week int) (map[string]sleepergen.StatMap, error) {
	key := fmt.Sprintf("stats:%s:%d", season, week)
	return cachedList(c, key, ttlStatMap, func() (*map[string]sleepergen.StatMap, *http.Response, []byte, error) {
		resp, err := c.gen.GetWeekStatsWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get stats %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// PlayersDump returns the full player map and rebuilds ResolvePlayer's index every call, so the index
// never lags a dump refetched after its TTL.
func (c *Client) PlayersDump(ctx context.Context) (map[string]sleepergen.Player, error) {
	out, err := cachedList(c, "players", ttlPlayersDump, func() (*map[string]sleepergen.Player, *http.Response, []byte, error) {
		resp, err := c.gen.GetAllPlayersWithResponse(ctx)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get players: %w", err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
	if err != nil {
		return nil, err
	}
	c.namesMu.Lock()
	c.names = buildNameIndex(out)
	c.namesMu.Unlock()
	return out, nil
}

// ResolvePlayer matches a name exactly (lowercased "first last", last name, or DEF team code) in the index
// PlayersDump last built; call PlayersDump first.
func (c *Client) ResolvePlayer(name string) []string {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	return c.names[strings.ToLower(strings.TrimSpace(name))]
}

// Chain walks previous_league_id back up to seasonsBack (0 = all), newest first, stopping at the first
// unreachable hop; callers want whatever history is reachable.
func (c *Client) Chain(ctx context.Context, leagueID string, seasonsBack int) []sleepergen.League {
	limit := maxChainSeasons
	if seasonsBack > 0 && seasonsBack < limit {
		limit = seasonsBack
	}
	var out []sleepergen.League
	id := leagueID
	for i := 0; i < limit && id != ""; i++ {
		league, err := c.League(ctx, id)
		if err != nil {
			break
		}
		out = append(out, *league)
		if league.PreviousLeagueId == nil {
			break
		}
		id = *league.PreviousLeagueId
	}
	return out
}

//nolint:dupl // each single-value cached lookup differs only by type and endpoint
func (c *Client) User(ctx context.Context, identifier string) (*sleepergen.User, error) {
	return cached(c, "user:"+identifier, ttlLeague, func() (*sleepergen.User, error) {
		resp, err := c.gen.GetUserWithResponse(ctx, identifier)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get user %s: %w", identifier, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

//nolint:dupl // see Rosters
func (c *Client) UserLeagues(ctx context.Context, userID, season string) ([]sleepergen.League, error) {
	key := fmt.Sprintf("user_leagues:%s:%s", userID, season)
	return cachedList(c, key, ttlLeague, func() (*[]sleepergen.League, *http.Response, []byte, error) {
		resp, err := c.gen.GetUserLeaguesWithResponse(ctx, userID, season)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get user leagues %s %s: %w", userID, season, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) Transactions(ctx context.Context, leagueID string, round int) ([]sleepergen.Transaction, error) {
	key := fmt.Sprintf("transactions:%s:%d", leagueID, round)
	return cachedList(c, key, ttlLeague, func() (*[]sleepergen.Transaction, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueTransactionsWithResponse(ctx, leagueID, round)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get transactions %s round %d: %w", leagueID, round, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) WinnersBracket(ctx context.Context, leagueID string) ([]sleepergen.BracketMatch, error) {
	return cachedList(c, "winners_bracket:"+leagueID, ttlLeague, func() (*[]sleepergen.BracketMatch, *http.Response, []byte, error) {
		resp, err := c.gen.GetWinnersBracketWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get winners bracket %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) LeagueDrafts(ctx context.Context, leagueID string) ([]sleepergen.Draft, error) {
	return cachedList(c, "league_drafts:"+leagueID, ttlLeague, func() (*[]sleepergen.Draft, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueDraftsWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get league drafts %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // each single-value cached lookup differs only by type and endpoint
func (c *Client) Draft(ctx context.Context, draftID string) (*sleepergen.Draft, error) {
	return cached(c, "draft:"+draftID, ttlLive, func() (*sleepergen.Draft, error) {
		resp, err := c.gen.GetDraftWithResponse(ctx, draftID)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get draft %s: %w", draftID, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

//nolint:dupl // see Rosters
func (c *Client) DraftPicks(ctx context.Context, draftID string) ([]sleepergen.DraftPick, error) {
	return cachedList(c, "draft_picks:"+draftID, ttlLive, func() (*[]sleepergen.DraftPick, *http.Response, []byte, error) {
		resp, err := c.gen.GetDraftPicksWithResponse(ctx, draftID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get draft picks %s: %w", draftID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

func (c *Client) TrendingPlayers(ctx context.Context, kind sleepergen.GetTrendingPlayersParamsType, lookbackHours, limit int) ([]sleepergen.TrendingPlayer, error) {
	key := fmt.Sprintf("trending:%s:%d:%d", kind, lookbackHours, limit)
	params := &sleepergen.GetTrendingPlayersParams{LookbackHours: &lookbackHours, Limit: &limit}
	return cachedList(c, key, ttlLive, func() (*[]sleepergen.TrendingPlayer, *http.Response, []byte, error) {
		resp, err := c.gen.GetTrendingPlayersWithResponse(ctx, kind, params)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get trending %s: %w", kind, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// Player is the cheaper single-lookup alternative to PlayersDump.
//
//nolint:dupl // each single-value cached lookup differs only by type and endpoint
func (c *Client) Player(ctx context.Context, playerID string) (*sleepergen.Player, error) {
	return cached(c, "player:"+playerID, ttlStatMap, func() (*sleepergen.Player, error) {
		resp, err := c.gen.GetPlayerWithResponse(ctx, playerID)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get player %s: %w", playerID, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

//nolint:dupl // see Rosters
func (c *Client) Schedule(ctx context.Context, season string) ([]sleepergen.Game, error) {
	return cachedList(c, "schedule:"+season, ttlLive, func() (*[]sleepergen.Game, *http.Response, []byte, error) {
		resp, err := c.gen.GetScheduleWithResponse(ctx, season)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get schedule %s: %w", season, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// WeekScores is read for kickoff and venue; its live-score fields are up to ttlKickoff stale.
//
//nolint:dupl // see Rosters
func (c *Client) WeekScores(ctx context.Context, season string, week int) ([]sleepergen.GameScore, error) {
	key := fmt.Sprintf("scores:%s:%d", season, week)
	return cachedList(c, key, ttlKickoff, func() (*[]sleepergen.GameScore, *http.Response, []byte, error) {
		resp, err := c.gen.GetWeekScoresWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get scores %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) Research(ctx context.Context, season string, week int) (map[string]sleepergen.ResearchEntry, error) {
	key := fmt.Sprintf("research:%s:%d", season, week)
	return cachedList(c, key, ttlStatMap, func() (*map[string]sleepergen.ResearchEntry, *http.Response, []byte, error) {
		resp, err := c.gen.GetPlayerResearchWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get research %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

func (c *Client) PlayerSeasonStats(ctx context.Context, playerID, season string) (*sleepergen.PlayerStatEntry, error) {
	return cached(c, "player_season_stats:"+playerID+":"+season, ttlStatMap, func() (*sleepergen.PlayerStatEntry, error) {
		params := &sleepergen.GetPlayerSeasonStatsParams{SeasonType: "regular", Season: season}
		resp, err := c.gen.GetPlayerSeasonStatsWithResponse(ctx, playerID, params)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get player season stats %s %s: %w", playerID, season, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

// PlayerGameLog adds the grouping=week param the generated call omits and decodes the per-week result.
func (c *Client) PlayerGameLog(ctx context.Context, playerID, season string) ([]sleepergen.PlayerStatEntry, error) {
	out, err := cached(c, "player_game_log:"+playerID+":"+season, ttlStatMap, func() ([]sleepergen.PlayerStatEntry, error) {
		return c.fetchPlayerGameLog(ctx, playerID, season)
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

func (c *Client) fetchPlayerGameLog(ctx context.Context, playerID, season string) ([]sleepergen.PlayerStatEntry, error) {
	raw, ok := c.gen.ClientInterface.(*sleepergen.Client)
	if !ok {
		return nil, fmt.Errorf("sleeper: unexpected generated client type %T", c.gen.ClientInterface)
	}
	params := &sleepergen.GetPlayerSeasonStatsParams{SeasonType: "regular", Season: season}
	req, err := sleepergen.NewGetPlayerSeasonStatsRequest(raw.Server, playerID, params)
	if err != nil {
		return nil, fmt.Errorf("sleeper: game log request %s %s: %w", playerID, season, err)
	}
	q := req.URL.Query()
	q.Set("grouping", "week")
	req.URL.RawQuery = q.Encode()
	resp, err := raw.Client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("sleeper: get game log %s %s: %w", playerID, season, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sleeper: read game log %s %s: %w", playerID, season, err)
	}
	// grouping=week returns {"1": entry, "2": entry, "3": null, ...}, keyed by week.
	var byWeek map[string]*sleepergen.PlayerStatEntry
	if resp.StatusCode == http.StatusNotFound || strings.TrimSpace(string(body)) == "null" {
		return nil, fmt.Errorf("%w (status %s): %s", ErrNotFound, resp.Status, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sleeper: unexpected response (status %s): %s", resp.Status, string(body))
	}
	if err := json.Unmarshal(body, &byWeek); err != nil {
		return nil, fmt.Errorf("sleeper: decode game log %s %s: %w", playerID, season, err)
	}
	entries := make([]sleepergen.PlayerStatEntry, 0, len(byWeek))
	for _, e := range byWeek {
		if e != nil {
			entries = append(entries, *e)
		}
	}
	return entries, nil
}

func (c *Client) SeasonStats(ctx context.Context, season string) (map[string]sleepergen.StatMap, error) {
	return cachedList(c, "season_stats:"+season, ttlStatMap, func() (*map[string]sleepergen.StatMap, *http.Response, []byte, error) {
		resp, err := c.gen.GetSeasonStatsWithResponse(ctx, season)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get season stats %s: %w", season, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// ErrNotFound wraps a real 404 or Sleeper's HTTP-200 literal-null body, so callers can tell it from a
// genuine fetch failure.
var ErrNotFound = errors.New("sleeper: not found")

// okJSON errors on a nil JSON200 or a literal "null" body (Sleeper's 200 for e.g. an unknown username),
// so a zero value is never cached.
func okJSON[T any](json *T, resp *http.Response, body []byte) (*T, error) {
	isNullBody := strings.TrimSpace(string(body)) == "null"
	if json != nil && !isNullBody {
		return json, nil
	}
	status, code := "unknown", 0
	if resp != nil {
		status, code = resp.Status, resp.StatusCode
	}
	if code == http.StatusNotFound || isNullBody {
		return nil, fmt.Errorf("%w (status %s): %s", ErrNotFound, status, string(body))
	}
	return nil, fmt.Errorf("sleeper: unexpected response (status %s): %s", status, string(body))
}

// buildNameIndex indexes by lowercased "first last", last name, and a DEF unit's team code (its player_id).
func buildNameIndex(players map[string]sleepergen.Player) map[string][]string {
	idx := map[string][]string{}
	add := func(key, id string) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		idx[key] = append(idx[key], id)
	}
	for id, p := range players {
		first, last := strVal(p.FirstName), strVal(p.LastName)
		if first != "" && last != "" {
			add(first+" "+last, id)
		}
		if last != "" {
			add(last, id)
		}
		if strVal(p.Position) == "DEF" {
			add(strVal(p.Team), id)
		}
	}
	return idx
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
