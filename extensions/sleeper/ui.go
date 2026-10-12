package sleeper

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
	"github.com/go-chi/chi/v5"
)

//go:embed ui/static
var uiStaticFS embed.FS

//go:embed ui/schemas
var uiSchemasFS embed.FS

// ArtifactSchemas implements sdk.ArtifactSchemas with the schemas the page
// itself reads by, so an agent's artifact is checked against its one consumer.
func (e *extension) ArtifactSchemas() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(artifactKinds))
	for _, kind := range artifactKinds {
		b, err := uiSchemasFS.ReadFile("ui/schemas/" + kind + ".json")
		if err != nil {
			panic("sleeper: embedded schema missing for artifact kind " + kind + ": " + err.Error())
		}
		out[kind] = json.RawMessage(b)
	}
	return out
}

// artifactKinds is every artifact name with a schema: the job ids plus season-notes and trade.
var artifactKinds = []string{"lineup", "waivers", "trade", "trade-finder", "digest", "trends", "retro", "draft", "history", "season-notes"}

func (e *extension) mountUI(authed chi.Router) {
	authed.Get("/api/seasons", e.handleSeasons)
	authed.Get("/api/season", e.handleSeason)
	authed.Get("/api/artifacts", e.handleArtifacts)
	authed.Get("/api/jobs", e.handleRunnableJobs)
	authed.Post("/api/jobs", e.handleJobs)

	static, err := fs.Sub(uiStaticFS, "ui/static")
	if err != nil {
		// A broken Sub on an embedded FS is a build-time bug (renamed directory), never runtime state.
		panic("sleeper: embedded UI assets missing: " + err.Error())
	}
	// chi's r.Mount("/"+name, combined) in quack's router does not strip the
	// prefix, so the file server must strip it itself or every asset 404s.
	fileServer := http.StripPrefix("/"+extensionName, http.FileServer(http.FS(static)))
	authed.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bare "/sleeper" (no trailing slash) would serve index.html at a path where
		// its relative asset/API refs resolve wrong; send it to the real page root.
		if r.URL.Path == "/"+extensionName {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		fileServer.ServeHTTP(w, r)
	}))
}

// writeJSON encodes to a buffer first so an unmarshalable value becomes a 500, not an empty 200.
func (e *extension) writeJSON(w http.ResponseWriter, v any) {
	e.writeJSONStatus(w, http.StatusOK, v)
}

func (e *extension) writeErr(w http.ResponseWriter, status int, msg string) {
	e.writeJSONStatus(w, status, map[string]string{"error": msg})
}

func (e *extension) writeJSONStatus(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		e.logError("sleeper: encode response failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"encode failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (e *extension) logError(msg string, args ...any) {
	if e.host.Log != nil {
		e.host.Log.Error(msg, args...)
		return
	}
	slog.Error(msg, args...)
}

// writeLeagueErr maps ErrNotFound to 404; anything else is an upstream failure (502).
func (e *extension) writeLeagueErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		e.writeErr(w, http.StatusNotFound, "unknown league")
		return
	}
	e.writeErr(w, http.StatusBadGateway, err.Error())
}

// resolveUserID resolves cfg.DefaultUser (username or user_id); !ok makes every "me"-scoped field absent.
func (e *extension) resolveUserID(ctx context.Context) (string, bool) {
	if e.cfg.DefaultUser == "" {
		return "", false
	}
	u, err := e.client.User(ctx, e.cfg.DefaultUser)
	if err != nil {
		return "", false
	}
	return u.UserId, true
}

func intSetting(m map[string]int, key string, def int) int {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func reserveSlots(positions []string) int {
	n := 0
	for _, p := range positions {
		if p == "IR" {
			n++
		}
	}
	return n
}

func scoringType(s map[string]float32) string {
	rec, ok := s["rec"]
	switch {
	case !ok:
		return ""
	case rec >= 1:
		return "PPR"
	case rec > 0:
		return "Half-PPR"
	default:
		return "Standard"
	}
}

func waiverTypeLabel(wt int) string {
	if wt == 2 {
		return "FAAB waivers"
	}
	return "Rolling waivers"
}

// recordString shows ties only when non-zero, so every surface formats a record the same way.
func recordString(wins, losses, ties int) string {
	if ties != 0 {
		return fmt.Sprintf("%d-%d-%d", wins, losses, ties)
	}
	return fmt.Sprintf("%d-%d", wins, losses)
}

func buildFacts(league *sleepergen.League) []string {
	facts := []string{fmt.Sprintf("%d teams", league.TotalRosters)}
	if s := scoringType(league.ScoringSettings); s != "" {
		facts = append(facts, s)
	}
	facts = append(facts, waiverTypeLabel(league.Settings["waiver_type"]))
	if pt, ok := league.Settings["playoff_teams"]; ok && pt > 0 {
		facts = append(facts, fmt.Sprintf("%d playoff spots · week %d", pt, league.Settings["playoff_week_start"]))
	}
	return facts
}

func usersByOwnerID(users []sleepergen.LeagueUser) map[string]sleepergen.LeagueUser {
	out := make(map[string]sleepergen.LeagueUser, len(users))
	for _, u := range users {
		out[u.UserId] = u
	}
	return out
}

func rosterByOwner(rosters []sleepergen.Roster, userID string) *sleepergen.Roster {
	for i := range rosters {
		if rosters[i].OwnerId != nil && *rosters[i].OwnerId == userID {
			return &rosters[i]
		}
	}
	return nil
}

type standingRow struct {
	ID     string  `json:"id"` // the owner's Sleeper user_id - stable, unlike team name
	Team   string  `json:"team"`
	Owner  string  `json:"owner"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Ties   int     `json:"ties"`
	PF     float64 `json:"pf"`
	PA     float64 `json:"pa"`
	Mine   bool    `json:"mine"`
}

func standingRowFor(ro sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) standingRow {
	var u sleepergen.LeagueUser
	var id string
	if ro.OwnerId != nil {
		u = usersByOwner[*ro.OwnerId]
		id = *ro.OwnerId
	}
	return standingRow{
		ID: id, Team: teamName(u), Owner: ownerName(u),
		Wins: ro.Settings["wins"], Losses: ro.Settings["losses"], Ties: ro.Settings["ties"],
		PF: pointsField(ro.Settings, "fpts"), PA: pointsField(ro.Settings, "fpts_against"),
		Mine: myRoster != nil && ro.RosterId == myRoster.RosterId,
	}
}

func buildStandings(rosters []sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) []standingRow {
	out := make([]standingRow, 0, len(rosters))
	for _, ro := range rosters {
		out = append(out, standingRowFor(ro, usersByOwner, myRoster))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wins != out[j].Wins {
			return out[i].Wins > out[j].Wins
		}
		return out[i].PF > out[j].PF
	})
	return out
}

type opponentInfo struct {
	Team   string `json:"team"`
	Owner  string `json:"owner"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
}

// findOpponent is nil with no "me", a non-live season, or no pairing yet this week (e.g. before week 1 locks).
func (e *extension) findOpponent(ctx context.Context, c *Client, league *sleepergen.League, rosters []sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) *opponentInfo {
	if myRoster == nil {
		return nil
	}
	st, err := c.State(ctx)
	if err != nil || st.Season != league.Season {
		return nil
	}
	matchups, err := c.Matchups(ctx, league.LeagueId, st.Week)
	if err != nil {
		return nil
	}
	mine, ok := findMatchup(matchups, myRoster.RosterId)
	if !ok {
		return nil
	}
	opp, ok := opponent(matchups, mine)
	if !ok {
		return nil
	}
	oppRoster, ok := rosterFor(rosters, opp.RosterId)
	if !ok || oppRoster.OwnerId == nil {
		return nil
	}
	u := usersByOwner[*oppRoster.OwnerId]
	return &opponentInfo{Team: teamName(u), Owner: ownerName(u), Wins: oppRoster.Settings["wins"], Losses: oppRoster.Settings["losses"]}
}

func playerNames(m *map[string]int, players map[string]sleepergen.Player) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for id := range *m {
		out = append(out, playerName(players, id))
	}
	sort.Strings(out)
	return out
}

func teamsFor(ids []int, ownerName map[int]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := ownerName[id]; ok {
			out = append(out, n)
		}
	}
	return out
}

func moveLabel(txType string, adds, drops []string) string {
	switch {
	case txType == "trade":
		return "Trade"
	case len(adds) > 0:
		return "Add " + adds[0]
	case len(drops) > 0:
		return "Drop " + drops[0]
	default:
		return "Move"
	}
}

type moveRow struct {
	Type  string   `json:"type"`
	Week  int      `json:"week"`
	Label string   `json:"label"`
	Adds  []string `json:"adds"`
	Drops []string `json:"drops"`
	By    []string `json:"by"`
}

func moveRowFor(tx sleepergen.Transaction, week int, players map[string]sleepergen.Player, ownerName map[int]string) moveRow {
	adds := playerNames(tx.Adds, players)
	drops := playerNames(tx.Drops, players)
	return moveRow{
		Type: tx.Type, Week: week, Label: moveLabel(tx.Type, adds, drops),
		Adds: adds, Drops: drops, By: teamsFor(tx.RosterIds, ownerName),
	}
}

// buildMoves is best-effort: an upstream failure yields no moves rather than failing the season response.
func (e *extension) buildMoves(ctx context.Context, c *Client, leagueID string, week int, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) []moveRow {
	if week < 1 {
		week = 1
	}
	txs, err := c.Transactions(ctx, leagueID, week)
	if err != nil {
		return nil
	}
	players, _ := c.PlayersDump(ctx)
	ownerName := teamNames(rosters, users)
	moves := make([]moveRow, 0, len(txs))
	for _, tx := range txs {
		moves = append(moves, moveRowFor(tx, week, players, ownerName))
	}
	return moves
}

type leagueFacts struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Season  string `json:"season"`
	WeekNow int    `json:"week_now"`
	Status  string `json:"status"`
}

type meInfo struct {
	Team      string `json:"team"`
	Owner     string `json:"owner"`
	Wins      int    `json:"wins"`
	Losses    int    `json:"losses"`
	Ties      int    `json:"ties"`
	WaiverPos int    `json:"waiver_pos"`
}

type seasonResponse struct {
	League       leagueFacts   `json:"league"`
	Facts        []string      `json:"facts"`
	Me           *meInfo       `json:"me"`
	Opponent     *opponentInfo `json:"opponent"`
	Standings    []standingRow `json:"standings"`
	RecentMoves  []moveRow     `json:"recent_moves"`
	ReserveSlots int           `json:"reserve_slots"`
	PlayoffLine  int           `json:"playoff_line"`
}

// handleSeason serves GET /sleeper/api/season?league_id=: live league/me/opponent/standings/moves, never artifacts.
func (e *extension) handleSeason(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := cmp.Or(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	if leagueID == "" {
		e.writeErr(w, http.StatusBadRequest, "league_id is required")
		return
	}
	c := e.client
	league, err := c.League(ctx, leagueID)
	if err != nil {
		e.writeLeagueErr(w, err)
		return
	}
	rosters, err := c.Rosters(ctx, leagueID)
	if err != nil {
		e.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	users, err := c.LeagueUsers(ctx, leagueID)
	if err != nil {
		e.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	usersByOwner := usersByOwnerID(users)
	weekNow := 0
	if st, err := c.State(ctx); err == nil {
		weekNow = st.Week
	}

	myRoster := e.findMyRoster(ctx, rosters)
	resp := seasonResponse{
		League:       leagueFacts{ID: league.LeagueId, Name: league.Name, Season: league.Season, WeekNow: weekNow, Status: league.Status},
		Facts:        buildFacts(league),
		Standings:    buildStandings(rosters, usersByOwner, myRoster),
		ReserveSlots: reserveSlots(league.RosterPositions),
		PlayoffLine:  intSetting(league.Settings, "playoff_teams", 6),
		RecentMoves:  e.buildMoves(ctx, c, leagueID, weekNow, rosters, users),
	}
	if myRoster != nil {
		u := usersByOwner[*myRoster.OwnerId]
		resp.Me = &meInfo{Team: teamName(u), Owner: ownerName(u), Wins: myRoster.Settings["wins"], Losses: myRoster.Settings["losses"], Ties: myRoster.Settings["ties"], WaiverPos: myRoster.Settings["waiver_position"]}
		resp.Opponent = e.findOpponent(ctx, c, league, rosters, usersByOwner, myRoster)
	}
	e.writeJSON(w, resp)
}

func (e *extension) findMyRoster(ctx context.Context, rosters []sleepergen.Roster) *sleepergen.Roster {
	userID, ok := e.resolveUserID(ctx)
	if !ok {
		return nil
	}
	return rosterByOwner(rosters, userID)
}

type seasonSummary struct {
	Season   string `json:"season"`
	LeagueID string `json:"league_id"`
	Wins     int    `json:"wins"`
	Losses   int    `json:"losses"`
	Status   string `json:"status"`
}

type seasonsResponse struct {
	CurrentSeason string          `json:"current_season"`
	Seasons       []seasonSummary `json:"seasons"`
}

func (e *extension) seasonSummaryFor(ctx context.Context, c *Client, lg sleepergen.League, userID string, haveUser bool) seasonSummary {
	out := seasonSummary{Season: lg.Season, LeagueID: lg.LeagueId, Status: lg.Status}
	if !haveUser {
		return out
	}
	rosters, err := c.Rosters(ctx, lg.LeagueId)
	if err != nil {
		return out
	}
	if ro := rosterByOwner(rosters, userID); ro != nil {
		out.Wins, out.Losses = ro.Settings["wins"], ro.Settings["losses"]
	}
	return out
}

// handleSeasons serves GET /sleeper/api/seasons?league_id=: the season chain, oldest first, with the default
// user's record in each. Chain walks newest-first, so it is iterated backwards.
func (e *extension) handleSeasons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := cmp.Or(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	if leagueID == "" {
		e.writeErr(w, http.StatusBadRequest, "league_id is required")
		return
	}
	c := e.client
	// Chain swallows failures, so fetch League first to tell an unknown league from a 5xx.
	if _, err := c.League(ctx, leagueID); err != nil {
		e.writeLeagueErr(w, err)
		return
	}
	chain := c.Chain(ctx, leagueID, 0)
	if len(chain) == 0 {
		e.writeErr(w, http.StatusNotFound, "unknown league")
		return
	}
	resp := seasonsResponse{CurrentSeason: chain[0].Season}
	userID, haveUser := e.resolveUserID(ctx)
	for _, lg := range slices.Backward(chain) {
		resp.Seasons = append(resp.Seasons, e.seasonSummaryFor(ctx, c, lg, userID, haveUser))
	}
	e.writeJSON(w, resp)
}

type artifactEnvelope struct {
	Found   bool            `json:"found"`
	Running bool            `json:"running"`
	Data    json.RawMessage `json:"data,omitempty"`
	// Invalid marks a non-JSON artifact (agent wrote prose); Text carries the raw content and Data is omitted.
	Invalid bool   `json:"invalid,omitempty"`
	Text    string `json:"text,omitempty"`
}

// maxInvalidTextBytes caps an invalid artifact's raw text so an agent runaway can't balloon the response.
const maxInvalidTextBytes = 64 * 1024

// readArtifact reads the job chat's artifact; non-JSON comes back Invalid.
func (e *extension) readArtifact(chatID, name string) artifactEnvelope {
	if e.host.ReadArtifact != nil {
		if data, ok := e.host.ReadArtifact(chatID, e.cfg.DefaultUser, name); ok {
			if parsed, ok := parseArtifactJSON(data); ok {
				return artifactEnvelope{Found: true, Data: parsed}
			}
			return artifactEnvelope{Found: true, Invalid: true, Text: capText(data, maxInvalidTextBytes)}
		}
	}
	return artifactEnvelope{Found: false}
}

// parseArtifactJSON accepts raw JSON, or JSON in one ``` / ```json fence since agents often fence output.
func parseArtifactJSON(data []byte) (json.RawMessage, bool) {
	if json.Valid(data) {
		return json.RawMessage(data), true
	}
	if stripped, ok := stripCodeFence(data); ok && json.Valid(stripped) {
		return json.RawMessage(stripped), true
	}
	return nil, false
}

func stripCodeFence(data []byte) ([]byte, bool) {
	s := strings.TrimSpace(string(data))
	if !strings.HasPrefix(s, "```") {
		return nil, false
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return []byte(strings.TrimSpace(s)), true
}

// capText truncates at a rune boundary so the capped text stays valid UTF-8.
func capText(data []byte, max int) string {
	if len(data) <= max {
		return string(data)
	}
	for max > 0 && !utf8.RuneStart(data[max]) {
		max--
	}
	return string(data[:max])
}

type tradeTalkEnvelope struct {
	Partner   string `json:"partner"`    // display team name
	PartnerID string `json:"partner_id"` // owner's Sleeper user_id; the chat id and dispatch key on it
	artifactEnvelope
}

// teamRef is one other league member; ID is the owner's user_id, stable across a team rename.
type teamRef struct {
	ID   string
	Name string
}

func (e *extension) otherTeams(ctx context.Context, leagueID string) []teamRef {
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return nil
	}
	myID, haveMe := e.resolveUserID(ctx)
	out := make([]teamRef, 0, len(users))
	for _, u := range users {
		if haveMe && u.UserId == myID {
			continue
		}
		out = append(out, teamRef{ID: u.UserId, Name: teamName(u)})
	}
	return out
}

// readTradeTalks probes every other team as a trade partner; the SDK has no chat-listing call.
func (e *extension) readTradeTalks(ctx context.Context, leagueID string) []tradeTalkEnvelope {
	var out []tradeTalkEnvelope
	for _, t := range e.otherTeams(ctx, leagueID) {
		chatID := fmt.Sprintf("ext:sleeper:%s:trade:%s", leagueID, t.ID)
		env := e.readArtifact(chatID, "trade")
		running := e.isRunning(chatID)
		switch {
		case env.Found:
			env.Running = running
			out = append(out, tradeTalkEnvelope{Partner: t.Name, PartnerID: t.ID, artifactEnvelope: env})
		case running:
			// A first talk has no artifact yet; without this the Running badge never shows.
			out = append(out, tradeTalkEnvelope{Partner: t.Name, PartnerID: t.ID, artifactEnvelope: artifactEnvelope{Running: true}})
		}
	}
	return out
}

// jobsForStop is the closed set of artifact-backed jobs per stop; only week stops allow trade talks.
func jobsForStop(stop string) (jobs []string, talksAllowed bool, err error) {
	switch stop {
	case "draft":
		return []string{"draft"}, false, nil
	case "review":
		return []string{"history"}, false, nil
	default:
		wk, convErr := strconv.Atoi(stop)
		if convErr != nil || wk < 1 || wk > 18 {
			return nil, false, fmt.Errorf("stop must be \"draft\", \"review\", or a week number 1-18, got %q", stop)
		}
		return []string{"lineup", "waivers", "digest", "trends", "retro", "trade-finder"}, true, nil
	}
}

type artifactsResponse struct {
	Stop        string                      `json:"stop"`
	Jobs        map[string]artifactEnvelope `json:"jobs"`
	Talks       []tradeTalkEnvelope         `json:"talks,omitempty"`
	SeasonNotes *artifactEnvelope           `json:"season_notes"`
}

// handleArtifacts serves GET /sleeper/api/artifacts?league_id=&stop=: the stop's job artifacts, season notes,
// and (week stops) trade talks.
func (e *extension) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := cmp.Or(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	stop := r.URL.Query().Get("stop")
	if leagueID == "" || stop == "" {
		e.writeErr(w, http.StatusBadRequest, "league_id and stop are required")
		return
	}
	jobs, talksAllowed, err := jobsForStop(stop)
	if err != nil {
		e.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	out := artifactsResponse{Stop: stop, Jobs: make(map[string]artifactEnvelope, len(jobs))}
	for _, job := range jobs {
		chatID := fmt.Sprintf("ext:sleeper:%s:%s:%s", leagueID, stop, job)
		env := e.readArtifact(chatID, job)
		env.Running = e.isRunning(chatID)
		out.Jobs[job] = env
	}
	notesChatID := fmt.Sprintf("ext:sleeper:%s:season-notes", leagueID)
	notes := e.readArtifact(notesChatID, "season-notes")
	notes.Running = e.isRunning(notesChatID)
	out.SeasonNotes = &notes
	if talksAllowed {
		out.Talks = e.readTradeTalks(ctx, leagueID)
	}
	e.writeJSON(w, out)
}

// jobWorkflows maps a job id to its workflow; a job with no agent yet is absent and not runnable.
var jobWorkflows = map[string]string{
	"lineup":       "sleeper-lineup",
	"waivers":      "sleeper-waivers",
	"trends":       "sleeper-trends",
	"trade":        "sleeper-trade",
	"digest":       "sleeper-digest",
	"retro":        "sleeper-retro",
	"draft":        "sleeper-draft",
	"history":      "sleeper-history",
	"trade-finder": "sleeper-trade-finder",
}

type jobRequest struct {
	LeagueID string            `json:"league_id"`
	Stop     string            `json:"stop"`
	Job      string            `json:"job"`
	Args     map[string]string `json:"args"`
}

type jobResponse struct {
	ChatID  string `json:"chat_id"`
	ChatURL string `json:"chat_url"`
}

func jobIsValid(job string, jobs []string, talksAllowed bool) bool {
	if job == "trade" {
		return talksAllowed
	}
	return slices.Contains(jobs, job)
}

func localIDFor(req jobRequest) (localID, title string, err error) {
	if req.Job == "trade" {
		partner := req.Args["partner"]
		if partner == "" {
			return "", "", fmt.Errorf("args.partner is required for the trade job")
		}
		// partner is the stable chat-id key; partner_name is display-only.
		name := cmp.Or(req.Args["partner_name"], partner)
		return fmt.Sprintf("%s:trade:%s", req.LeagueID, partner), fmt.Sprintf("Sleeper trade talk with %s", name), nil
	}
	return fmt.Sprintf("%s:%s:%s", req.LeagueID, req.Stop, req.Job), fmt.Sprintf("Sleeper %s, %s", req.Job, req.Stop), nil
}

// formatArgs renders non-empty args, sorted, so the analyst sees e.g. a trade's partner/give/get.
func formatArgs(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if args[k] != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", k, args[k])
	}
	return " " + strings.Join(parts, "; ") + "."
}

type runnableJobsResponse struct {
	Runnable []string `json:"runnable"`
}

// handleRunnableJobs serves GET /sleeper/api/jobs so the UI can disable Run for an unbound job.
func (e *extension) handleRunnableJobs(w http.ResponseWriter, r *http.Request) {
	e.writeJSON(w, runnableJobsResponse{Runnable: slices.Sorted(maps.Keys(jobWorkflows))})
}

// handleJobs serves POST /sleeper/api/jobs {league_id, stop, job, args}: dispatches to (or appends a turn on)
// the chat handleArtifacts reads, and returns its link.
func (e *extension) handleJobs(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		e.writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	req.LeagueID = cmp.Or(req.LeagueID, e.cfg.DefaultLeague)
	if req.LeagueID == "" || req.Stop == "" || req.Job == "" {
		e.writeErr(w, http.StatusBadRequest, "league_id, stop, and job are required")
		return
	}
	jobs, talksAllowed, err := jobsForStop(req.Stop)
	if err != nil {
		e.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !jobIsValid(req.Job, jobs, talksAllowed) {
		e.writeErr(w, http.StatusBadRequest, fmt.Sprintf("job %q is not valid for stop %q", req.Job, req.Stop))
		return
	}
	localID, title, err := localIDFor(req)
	if err != nil {
		e.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if e.host.Dispatch == nil {
		e.writeErr(w, http.StatusServiceUnavailable, "dispatch is not available")
		return
	}
	ctx := r.Context()
	partnerName := ""
	if req.Job == "trade" {
		partnerName = cmp.Or(req.Args["partner_name"], req.Args["partner"])
	}
	origin := e.jobOrigin(ctx, req.LeagueID, req.Stop, req.Job, originLabel(req.Stop, req.Job, partnerName))
	message := fmt.Sprintf("Run the %s job for league %s, stop %s.%s", req.Job, req.LeagueID, req.Stop, formatArgs(req.Args))
	chatID := globalChatID(localID)
	err = e.dispatchTracked(ctx, sdk.DispatchRequest{
		Chat: sdk.ChatRef{LocalID: localID, User: e.cfg.DefaultUser, Title: title, Origin: origin},
		Ask:  sdk.Ask{Message: message},
		Run:  sdk.RunConfig{ReadOnly: true, Workflow: jobWorkflows[req.Job]},
	}, chatID)
	if err != nil {
		e.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Job == "trends" {
		e.dispatchSeasonNotes(ctx, req.LeagueID)
	}
	e.writeJSON(w, jobResponse{ChatID: chatID, ChatURL: "/chat/" + chatID})
}
