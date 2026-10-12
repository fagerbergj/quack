package sleeper

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

// fakeHost records ReadArtifact/Dispatch calls so tests can assert on chat ids and turn appends.
type fakeHost struct {
	artifacts   map[string]map[string][]byte // chatID -> name -> data
	dispatched  []sdk.DispatchRequest
	dispatchErr error  // returned by every dispatch call when set
	onDispatch  func() // called synchronously inside dispatch, before it returns
}

func (h *fakeHost) readArtifact(chatID, user, name string) ([]byte, bool) {
	byName, ok := h.artifacts[chatID]
	if !ok {
		return nil, false
	}
	data, ok := byName[name]
	return data, ok
}

func (h *fakeHost) dispatch(_ context.Context, req sdk.DispatchRequest) error {
	h.dispatched = append(h.dispatched, req)
	if h.onDispatch != nil {
		h.onDispatch()
	}
	return h.dispatchErr
}

func (h *fakeHost) sdkHost() sdk.Host {
	return sdk.Host{ReadArtifact: h.readArtifact, Dispatch: h.dispatch}
}

func newTestExtension(t *testing.T, host sdk.Host, cfg config) (*extension, *chi.Mux) {
	t.Helper()
	cfg.DefaultUser = cmp.Or(cfg.DefaultUser, testUser)
	e := &extension{host: host, cfg: cfg, client: newTestClient(t)}
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())
	return e, r
}

func TestHandleSeasons(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id="+testLeague, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp seasonsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CurrentSeason != "2026" {
		t.Errorf("current_season = %q, want 2026", resp.CurrentSeason)
	}
	found := map[string]seasonSummary{}
	for _, s := range resp.Seasons {
		found[s.LeagueID] = s
	}
	cur, ok := found[testLeague]
	if !ok {
		t.Fatalf("chain missing current league %s: %+v", testLeague, resp.Seasons)
	}
	if cur.Wins != 0 || cur.Losses != 1 {
		t.Errorf("current league record = %d-%d, want 0-1", cur.Wins, cur.Losses)
	}
	if _, ok := found[pastLeague]; !ok {
		t.Errorf("chain missing past league %s: %+v", pastLeague, resp.Seasons)
	}
	// Chronological, current last (the approved design; the underlying walk is newest-first).
	if len(resp.Seasons) < 2 {
		t.Fatalf("expected at least 2 seasons, got %+v", resp.Seasons)
	}
	if resp.Seasons[len(resp.Seasons)-1].LeagueID != testLeague {
		t.Errorf("last season = %+v, want the current league last", resp.Seasons[len(resp.Seasons)-1])
	}
	if resp.Seasons[0].LeagueID == testLeague {
		t.Errorf("first season is the current league; want oldest first: %+v", resp.Seasons)
	}
}

func TestHandleSeasonsUnknownLeague(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id=9999999999999999999", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleSeasonsUpstreamFailureIsBadGateway: an upstream 5xx, unlike a 404 or null body, must not read
// as "unknown league".
func TestHandleSeasonsUpstreamFailureIsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	e := &extension{client: client, cfg: config{DefaultUser: testUser}}
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id="+testLeague, nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502, body = %s", rec.Code, rec.Body)
	}
}

func TestHandleSeasonsMissingLeagueID(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSeason(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/season?league_id="+testLeague, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp seasonResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.League.Name == "" {
		t.Error("league name is empty")
	}
	if len(resp.Standings) != 10 {
		t.Errorf("standings length = %d, want 10", len(resp.Standings))
	}
	if resp.Me == nil || resp.Me.Team != "Substation Supremacy" {
		t.Errorf("me = %+v, want team Substation Supremacy", resp.Me)
	}
	if resp.Opponent == nil {
		t.Error("opponent is nil, want the week-2 matchup opponent")
	} else if resp.Opponent.Team != "Pitts and Giggles" {
		t.Errorf("opponent team = %q, want Pitts and Giggles (roster 7, matchup_id 3)", resp.Opponent.Team)
	}
	if resp.ReserveSlots != 0 {
		t.Errorf("reserve_slots = %d, want 0 (this league's roster_positions has no IR slot)", resp.ReserveSlots)
	}
}

func TestHandleSeasonBadInput(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/season", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleArtifactsReadsExactChatID(t *testing.T) {
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		wantChatID: {"lineup": []byte(`{"week":2}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup, ok := resp.Jobs["lineup"]
	if !ok || !lineup.Found {
		t.Fatalf("jobs[lineup] = %+v, want found", resp.Jobs["lineup"])
	}
	if string(lineup.Data) != `{"week":2}` {
		t.Errorf("data = %s, want the stored bytes verbatim", lineup.Data)
	}
	for _, job := range []string{"waivers", "digest", "trends", "retro", "trade-finder"} {
		if resp.Jobs[job].Found {
			t.Errorf("jobs[%s] should not be found (no artifact stored)", job)
		}
	}
}

// TestHandleArtifactsIncludesTradeFinder pins the finder's own artifact
// kind: one chat per week, read back under jobs["trade-finder"].
func TestHandleArtifactsIncludesTradeFinder(t *testing.T) {
	chatID := "ext:sleeper:" + testLeague + ":2:trade-finder"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		chatID: {"trade-finder": []byte(`{"league":"` + testLeague + `","week":2,"suggestions":[]}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	finder, ok := resp.Jobs["trade-finder"]
	if !ok || !finder.Found {
		t.Fatalf("jobs[trade-finder] = %+v, want found", resp.Jobs["trade-finder"])
	}
}

func TestHandleArtifactsDraftAndReviewStops(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		"ext:sleeper:" + testLeague + ":draft:draft":    {"draft": []byte(`{"season":"2026"}`)},
		"ext:sleeper:" + testLeague + ":review:history": {"history": []byte(`{"season":"2025"}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=draft", nil))
	var draftResp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &draftResp)
	if !draftResp.Jobs["draft"].Found {
		t.Errorf("draft stop should read the draft artifact: %+v", draftResp.Jobs)
	}
	if len(draftResp.Talks) != 0 {
		t.Errorf("draft stop must not include trade talks, got %d", len(draftResp.Talks))
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=review", nil))
	var reviewResp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &reviewResp)
	if !reviewResp.Jobs["history"].Found {
		t.Errorf("review stop should read the history artifact: %+v", reviewResp.Jobs)
	}
}

func TestHandleArtifactsBadStop(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleArtifactsRealTradeTalkDiscovery exercises readTradeTalks' discovery over real league members.
func TestHandleArtifactsRealTradeTalkDiscovery(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840" // pirates5 / "Rice Cooker" in testLeague's fixture users
	chatID := "ext:sleeper:" + testLeague + ":trade:" + riceCookerOwnerID
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		chatID: {"trade": []byte(`{"partner":"Rice Cooker","partner_id":"` + riceCookerOwnerID + `","status":"open","offers":[]}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Talks) != 1 {
		t.Fatalf("talks = %+v, want exactly the one stored talk", resp.Talks)
	}
	got := resp.Talks[0]
	if got.PartnerID != riceCookerOwnerID || got.Partner != "Rice Cooker" || !got.Found {
		t.Errorf("talk = %+v, want partner_id=%q partner=Rice Cooker found=true example=false", got, riceCookerOwnerID)
	}
}

// TestHandleArtifactsRunningFirstTalkSurfaces: a partner chat with no artifact yet still shows Running,
// or a first talk never starts polling.
func TestHandleArtifactsRunningFirstTalkSurfaces(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840"
	chatID := "ext:sleeper:" + testLeague + ":trade:" + riceCookerOwnerID
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	e, r := newTestExtension(t, host.sdkHost(), config{})
	e.markRunning(chatID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Talks) != 1 {
		t.Fatalf("talks = %+v, want the one running-but-not-yet-found talk", resp.Talks)
	}
	got := resp.Talks[0]
	if got.PartnerID != riceCookerOwnerID || got.Found || !got.Running {
		t.Errorf("talk = %+v, want partner_id=%q found=false running=true", got, riceCookerOwnerID)
	}
}

func TestHandleArtifactsNoHostLeavesEmpty(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for job, env := range resp.Jobs {
		if env.Found {
			t.Errorf("jobs[%s] found with no host: %+v", job, env)
		}
	}
	if len(resp.Talks) != 0 {
		t.Errorf("talks = %+v, want none with no host", resp.Talks)
	}
}

func TestHandleJobsDispatchesChatIDAndAppendsTurn(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	e, r := newTestExtension(t, host.sdkHost(), config{})
	body := strings.NewReader(`{"league_id":"` + testLeague + `","stop":"2","job":"lineup"}`)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	if resp.ChatID != wantChatID {
		t.Errorf("chat_id = %q, want %q", resp.ChatID, wantChatID)
	}
	if resp.ChatURL != "/chat/"+wantChatID {
		t.Errorf("chat_url = %q, want /chat/%s", resp.ChatURL, wantChatID)
	}
	if host.dispatched[0].Run.Workflow != "sleeper-lineup" {
		t.Errorf("workflow = %q, want sleeper-lineup (bound, no planner call)", host.dispatched[0].Run.Workflow)
	}
	origin := host.dispatched[0].Chat.Origin
	if origin == nil {
		t.Fatal("Chat.Origin is nil, want a sidebar chip")
	}
	if origin.Extension != extensionName || origin.Kind != "lineup" || origin.Label != "Lineup · week 2" {
		t.Errorf("origin = %+v, want extension=sleeper kind=lineup label=%q", origin, "Lineup · week 2")
	}
	if origin.Badge != "Roger is a Clown" {
		t.Errorf("origin.Badge = %q, want the league name", origin.Badge)
	}
	if origin.Href != "/sleeper/?league_id="+testLeague+"&stop=2" {
		t.Errorf("origin.Href = %q, want a deep link back to this stop", origin.Href)
	}
	if got := origin.Labels["league"]; len(got) != 1 || got[0].Value != testLeague || got[0].Display != "Roger is a Clown" {
		t.Errorf("origin.Labels[league] = %+v, want one value=%s display=Roger is a Clown", got, testLeague)
	}
	if got := origin.Labels["job"]; len(got) != 1 || got[0].Value != "lineup" {
		t.Errorf("origin.Labels[job] = %+v, want one value=lineup", got)
	}
	if !e.isRunning(wantChatID) {
		t.Error("chat should be marked running right after a successful dispatch")
	}

	// A second POST for the same job/stop must target the same LocalID, so
	// the host appends a turn instead of starting a second chat.
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"league_id":"`+testLeague+`","stop":"2","job":"lineup"}`)))
	if len(host.dispatched) != 2 {
		t.Fatalf("dispatched %d requests, want 2", len(host.dispatched))
	}
	if host.dispatched[0].Chat.LocalID != host.dispatched[1].Chat.LocalID {
		t.Errorf("repeat dispatch used a different LocalID: %q vs %q", host.dispatched[0].Chat.LocalID, host.dispatched[1].Chat.LocalID)
	}
}

// TestHandleJobsEveryMappedJobBindsItsWorkflow pins jobWorkflows exactly:
// every job id quack has an agent for names its bound shape, none guess via the planner.
func TestHandleJobsEveryMappedJobBindsItsWorkflow(t *testing.T) {
	// Literal expectations, not jobWorkflows itself: the names are quack config keys.
	want := map[string]string{
		"lineup": "sleeper-lineup", "waivers": "sleeper-waivers", "trends": "sleeper-trends",
		"digest": "sleeper-digest", "retro": "sleeper-retro", "draft": "sleeper-draft",
		"history": "sleeper-history", "trade": "sleeper-trade", "trade-finder": "sleeper-trade-finder",
	}
	if len(want) != len(jobWorkflows) {
		t.Fatalf("jobWorkflows has %d entries, this test pins %d", len(jobWorkflows), len(want))
	}
	stopFor := map[string]string{"draft": "draft", "history": "review"}
	for job, want := range want {
		t.Run(job, func(t *testing.T) {
			stop := cmp.Or(stopFor[job], "2")
			body := `{"league_id":"` + testLeague + `","stop":"` + stop + `","job":"` + job + `"}`
			if job == "trade" {
				body = `{"league_id":"` + testLeague + `","stop":"2","job":"trade","args":{"partner":"740613226189987840"}}`
			}
			host := &fakeHost{artifacts: map[string]map[string][]byte{}}
			_, r := newTestExtension(t, host.sdkHost(), config{})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if len(host.dispatched) == 0 {
				t.Fatal("nothing dispatched")
			}
			if got := host.dispatched[0].Run.Workflow; got != want {
				t.Errorf("%s workflow = %q, want %q", job, got, want)
			}
		})
	}
}

// TestHandleJobsTradeDispatchCarriesArgs: the trade dispatch message carries partner/partner_name/give/get.
func TestHandleJobsTradeDispatchCarriesArgs(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840"
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trade","args":{"partner":"` + riceCookerOwnerID + `","partner_name":"Rice Cooker","give":"7594","get":"9997"}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(host.dispatched) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(host.dispatched))
	}
	req := host.dispatched[0]
	if req.Run.Workflow != "sleeper-trade" {
		t.Errorf("workflow = %q, want sleeper-trade", req.Run.Workflow)
	}
	for _, want := range []string{"partner: " + riceCookerOwnerID, "partner_name: Rice Cooker", "give: 7594", "get: 9997"} {
		if !strings.Contains(req.Ask.Message, want) {
			t.Errorf("message %q missing %q", req.Ask.Message, want)
		}
	}
}

// TestHandleJobsTradeFinderDispatchesToOwnChat pins the finder's chat shape:
// one chat per week, separate from any per-partner talk.
func TestHandleJobsTradeFinderDispatchesToOwnChat(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trade-finder"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantChatID := "ext:sleeper:" + testLeague + ":2:trade-finder"
	if resp.ChatID != wantChatID {
		t.Errorf("chat_id = %q, want %q", resp.ChatID, wantChatID)
	}
	if len(host.dispatched) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(host.dispatched))
	}
	if got := host.dispatched[0].Chat.LocalID; got != testLeague+":2:trade-finder" {
		t.Errorf("LocalID = %q, want %q", got, testLeague+":2:trade-finder")
	}
	if got := host.dispatched[0].Run.Workflow; got != "sleeper-trade-finder" {
		t.Errorf("workflow = %q, want sleeper-trade-finder", got)
	}
}

func TestHandleRunnableJobsMatchesJobWorkflows(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp runnableJobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runnable) != len(jobWorkflows) {
		t.Fatalf("runnable = %v, want exactly jobWorkflows' keys (%d)", resp.Runnable, len(jobWorkflows))
	}
	for _, job := range resp.Runnable {
		if _, ok := jobWorkflows[job]; !ok {
			t.Errorf("runnable lists %q, not a jobWorkflows key", job)
		}
	}
}

func TestLocalIDForTradeRequiresPartner(t *testing.T) {
	_, _, err := localIDFor(jobRequest{LeagueID: testLeague, Stop: "2", Job: "trade"})
	if err == nil {
		t.Error("want an error for a trade job with no args.partner")
	}
}

func TestLocalIDForTradeKeysOnUserIDNotName(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840"
	req := jobRequest{LeagueID: testLeague, Stop: "2", Job: "trade", Args: map[string]string{"partner": riceCookerOwnerID, "partner_name": "Rice Cooker"}}
	localID, title, err := localIDFor(req)
	if err != nil {
		t.Fatalf("localIDFor: %v", err)
	}
	wantLocalID := testLeague + ":trade:" + riceCookerOwnerID
	if localID != wantLocalID {
		t.Errorf("localID = %q, want %q (keyed on the user_id, not the display name)", localID, wantLocalID)
	}
	wantTitle := "Sleeper trade talk with Rice Cooker"
	if title != wantTitle {
		t.Errorf("title = %q, want %q", title, wantTitle)
	}
}

func TestHandleJobsArgsReachTheMessage(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"waivers","args":{"partner":"Rice Cooker","give":"7594,8142","get":"9997"}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(host.dispatched) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(host.dispatched))
	}
	msg := host.dispatched[0].Ask.Message
	for _, want := range []string{"partner: Rice Cooker", "give: 7594,8142", "get: 9997"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestHandleJobsTrendsAlsoDispatchesSeasonNotes(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trends"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(host.dispatched) != 2 {
		t.Fatalf("dispatched %d requests, want 2 (trends + season-notes): %+v", len(host.dispatched), host.dispatched)
	}
	wantLocalID := testLeague + ":season-notes"
	if host.dispatched[1].Chat.LocalID != wantLocalID {
		t.Errorf("second dispatch LocalID = %q, want %q", host.dispatched[1].Chat.LocalID, wantLocalID)
	}
	if host.dispatched[0].Run.Workflow != "sleeper-trends" {
		t.Errorf("trends workflow = %q, want sleeper-trends", host.dispatched[0].Run.Workflow)
	}
	if host.dispatched[1].Run.Workflow != "sleeper-season-notes" {
		t.Errorf("season-notes workflow = %q, want sleeper-season-notes", host.dispatched[1].Run.Workflow)
	}
	trendsOrigin, notesOrigin := host.dispatched[0].Chat.Origin, host.dispatched[1].Chat.Origin
	if trendsOrigin == nil || trendsOrigin.Kind != "trends" || trendsOrigin.Label != "Trends · week 2" {
		t.Errorf("trends origin = %+v, want kind=trends label=%q", trendsOrigin, "Trends · week 2")
	}
	if notesOrigin == nil || notesOrigin.Kind != "season-notes" || notesOrigin.Label != "Season notes" {
		t.Errorf("season-notes origin = %+v, want kind=season-notes label=%q", notesOrigin, "Season notes")
	}
}

func TestHandleJobsBadJobForStop(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	body := `{"league_id":"` + testLeague + `","stop":"draft","job":"lineup"}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleJobsBadInput(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestUIServesUnderQuackMount mounts like quack's router (r.Mount("/"+name, ...), which chi does not
// strip); the page and assets must still resolve.
func TestUIServesUnderQuackMount(t *testing.T) {
	e := &extension{host: sdk.Host{}, cfg: config{DefaultUser: testUser, DefaultLeague: testLeague}, client: newTestClient(t)}
	combined := chi.NewRouter()
	e.RegisterRoutes(combined, combined)
	r := chi.NewRouter()
	r.Mount("/"+extensionName, combined)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sleeper/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sleeper/ status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET /sleeper/ content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<title>Sleeper") {
		t.Errorf("index page missing expected title, got: %s", rec.Body.String()[:min(200, rec.Body.Len())])
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sleeper/main.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sleeper/main.js status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sleeper/api/artifacts?stop=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sleeper/api/artifacts?stop=1 status = %d, body = %s", rec.Code, rec.Body)
	}

	// Bare "/sleeper" (no trailing slash) must redirect, not serve index.html at a
	// path where its relative asset/API refs would resolve wrong.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sleeper", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /sleeper status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/sleeper/" {
		t.Errorf("GET /sleeper Location = %q, want /sleeper/", loc)
	}
}

// TestHandleArtifactsNonJSONArtifact: a markdown artifact still 200s, marked invalid with the raw text.
func TestHandleArtifactsNonJSONArtifact(t *testing.T) {
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	const md = "# Lineup\n\nStart Josh Allen.\n"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		wantChatID: {"lineup": []byte(md)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty body (the original bug)")
	}
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup := resp.Jobs["lineup"]
	if !lineup.Found || !lineup.Invalid {
		t.Fatalf("lineup = %+v, want found+invalid", lineup)
	}
	if lineup.Text != md {
		t.Errorf("text = %q, want %q", lineup.Text, md)
	}
	if len(lineup.Data) != 0 {
		t.Errorf("data = %s, want absent for an invalid artifact", lineup.Data)
	}
}

// TestHandleArtifactsFencedJSONArtifact: a ```json fence around valid JSON parses as Data, not Invalid.
func TestHandleArtifactsFencedJSONArtifact(t *testing.T) {
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	fenced := "```json\n{\"week\":2}\n```"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		wantChatID: {"lineup": []byte(fenced)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup := resp.Jobs["lineup"]
	if !lineup.Found || lineup.Invalid {
		t.Fatalf("lineup = %+v, want found and not invalid (fence stripped)", lineup)
	}
	if string(lineup.Data) != `{"week":2}` {
		t.Errorf("data = %s, want the unfenced JSON", lineup.Data)
	}

	// A bare ``` fence (no "json" tag) must strip the same way.
	bareFenced := "```\n{\"week\":3}\n```"
	host.artifacts[wantChatID]["lineup"] = []byte(bareFenced)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if lineup = resp.Jobs["lineup"]; !lineup.Found || lineup.Invalid || string(lineup.Data) != `{"week":3}` {
		t.Errorf("bare-fence lineup = %+v, want found, not invalid, data {\"week\":3}", lineup)
	}
}

func TestCapTextKeepsRuneBoundary(t *testing.T) {
	if got := capText([]byte("aé"), 2); got != "a" {
		t.Errorf("capText split a rune: %q", got)
	}
	if got := capText([]byte("ab"), 5); got != "ab" {
		t.Errorf("capText under max = %q, want ab", got)
	}
}

// TestWriteJSONEncodeFailureIsNot200: an unencodable value (NaN) must 500 with an error body, not an
// empty 200.
func TestWriteJSONEncodeFailureIsNot200(t *testing.T) {
	e := &extension{}
	rec := httptest.NewRecorder()
	e.writeJSON(rec, struct {
		NaN float64 `json:"nan"`
	}{NaN: math.NaN()})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty body (the original bug: an encode failure must not ship an empty 200)")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("body = %v, want a non-empty error message", body)
	}
}

// Kinds come from what the extension dispatches, not from the list
// ArtifactSchemas itself walks, so a new job without a schema fails here.
func TestArtifactSchemasCoverEveryKind(t *testing.T) {
	kinds := map[string]bool{"trade": true, "season-notes": true}
	for _, stop := range []string{"draft", "review", "1"} {
		jobs, _, err := jobsForStop(stop)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			kinds[j] = true
		}
	}
	got := (&extension{}).ArtifactSchemas()
	for kind := range kinds {
		var doc map[string]any
		if err := json.Unmarshal(got[kind], &doc); err != nil {
			t.Errorf("kind %q: no parseable schema: %v", kind, err)
		}
	}
}
