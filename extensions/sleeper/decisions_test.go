package sleeper

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

//go:embed ui/fixtures
var uiFixturesFS embed.FS

// fixtureBytes holds the reference artifacts the storybook stories also render.
var fixtureBytes = func() map[string][]byte {
	out := map[string][]byte{}
	for _, name := range artifactKinds {
		if b, err := uiFixturesFS.ReadFile("ui/fixtures/" + name + ".json"); err == nil {
			out[name] = b
		}
	}
	return out
}()

// decideRecorder is a fake Host.Decide that records each request.
type decideRecorder struct {
	mu   sync.Mutex
	reqs []sdk.DecideRequest
	err  error
	wait bool // block until the call's ctx ends
}

func (d *decideRecorder) decide(ctx context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	if d.wait {
		<-ctx.Done()
		return sdk.Decision{}, ctx.Err()
	}
	return sdk.Decision{Top: "false", Outcome: "observe", Act: true}, d.err
}

func (d *decideRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

// decideExt is a fixture-backed extension whose job chats hold the reference artifacts.
func decideExt(t *testing.T, rec *decideRecorder) (*extension, *fakeHost) {
	t.Helper()
	fh := &fakeHost{artifacts: map[string]map[string][]byte{
		"ext:sleeper:" + testLeague + ":3:lineup":        {"lineup": fixtureBytes["lineup"]},
		"ext:sleeper:" + testLeague + ":3:waivers":       {"waivers": fixtureBytes["waivers"]},
		"ext:sleeper:" + testLeague + ":3:digest":        {"digest": fixtureBytes["digest"]},
		"ext:sleeper:" + testLeague + ":trade:860317606": {"trade": fixtureBytes["trade"]},
	}}
	host := fh.sdkHost()
	if rec != nil {
		host.Decide = rec.decide
	}
	e, _ := newTestExtension(t, host, config{})
	return e, fh
}

func stateMap(t *testing.T, req sdk.DecideRequest) map[string]any {
	t.Helper()
	b, err := json.Marshal(req.State)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func noAnswerKeys(t *testing.T, req sdk.DecideRequest, keys ...string) {
	t.Helper()
	b, _ := json.Marshal(req.State)
	for _, k := range keys {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s state carries %q: %s", req.Point, k, b)
		}
	}
	if len(b) > 24000 {
		t.Errorf("%s state is %d bytes, want well under the handler's token cap", req.Point, len(b))
	}
}

func TestLineupChangePerSwap(t *testing.T) {
	rec := &decideRecorder{}
	e, _ := decideExt(t, rec)
	e.decideRun(context.Background(), "ext:sleeper:"+testLeague+":3:lineup")
	if len(rec.reqs) != 2 {
		t.Fatalf("lineup asked %d decisions, want one per replaced starter (2)", len(rec.reqs))
	}
	want := []struct{ slot, cur, prop string }{{"RB2", "7021", "7594"}, {"FLEX", "8148", "11584"}}
	for i, req := range rec.reqs {
		if req.Point != "lineup_change" || req.Baseline != "true" {
			t.Errorf("req %d = %s baseline %q, want lineup_change true", i, req.Point, req.Baseline)
		}
		st := stateMap(t, req)
		cur, prop := st["current"].(map[string]any), st["proposed"].(map[string]any)
		if st["slot"] != want[i].slot || cur["id"] != want[i].cur || prop["id"] != want[i].prop {
			t.Errorf("req %d slot/current/proposed = %v/%v/%v, want %+v", i, st["slot"], cur["id"], prop["id"], want[i])
		}
		if st["league_id"] != testLeague || st["week"] != float64(3) || st["season"] != "2026" || st["chat_id"] == "" {
			t.Errorf("req %d join keys = %v/%v/%v/%v", i, st["league_id"], st["week"], st["season"], st["chat_id"])
		}
		noAnswerKeys(t, req, "verdict", "replaces", "confidence", "starters")
	}
	cur := stateMap(t, rec.reqs[0])["current"].(map[string]any)
	if cur["floor"] != float64(5) || cur["status"] != "Questionable" {
		t.Errorf("current row lacks its bench range/status: %v", cur)
	}
}

func TestWaiverPickupAndPriority(t *testing.T) {
	rec := &decideRecorder{}
	e, _ := decideExt(t, rec)
	if _, err := e.client.PlayersDump(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.decideRun(context.Background(), "ext:sleeper:"+testLeague+":3:waivers")
	got := map[string][]string{}
	for _, req := range rec.reqs {
		st := stateMap(t, req)
		got[req.Point] = append(got[req.Point], st["add"].(map[string]any)["id"].(string)+"="+req.Baseline)
		noAnswerKeys(t, req, "rank", "also_checked", "candidates")
	}
	wantPickup := "13301=true 13302=true 5001=true 13303=false 8155=false 13304=false"
	if s := strings.Join(got["waiver_pickup"], " "); s != wantPickup {
		t.Errorf("waiver_pickup = %s, want %s", s, wantPickup)
	}
	// Ranks 1, 2, 3 map to levels 4, 3, 2; the requests go out in player-id order.
	if s := strings.Join(got["waiver_priority"], " "); s != "13301=4 13302=3 5001=2" {
		t.Errorf("waiver_priority = %s, want 13301=4 13302=3 5001=2", s)
	}
	first := stateMap(t, rec.reqs[0])
	drop, _ := first["drop"].(map[string]any)
	if drop["id"] != "7021" || drop["name"] != "Rico Dowdle" || drop["pos"] != "RB" || first["week"] != float64(3) {
		t.Errorf("pickup drop/week = %v/%v, want the Rico Dowdle (7021, RB) row and week 3", drop, first["week"])
	}
	prio := stateMap(t, rec.reqs[len(rec.reqs)-1])
	var nullProj map[string]any
	if err := json.Unmarshal(fixtureBytes["waivers"], &nullProj); err != nil {
		t.Fatal(err)
	}
	c0 := nullProj["candidates"].([]any)[0].(map[string]any)
	c0["player"].(map[string]any)["proj"] = nil
	var a waiversArtifact
	b, _ := json.Marshal(nullProj)
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	base := func(p artPlayer) playerRow { return playerRow{ID: p.ID, Proj: p.Proj} }
	if r := candidateRow(base, a.Candidates[0]); r.Proj == nil || *r.Proj != c0["proj"].(float64) {
		t.Errorf("a null player.proj row = %v, want the candidate's proj %v", r.Proj, c0["proj"])
	}
	if others := prio["other_candidates"].([]any); len(others) != 2 {
		t.Errorf("priority state lists %d other candidates, want 2", len(others))
	}
}

func TestTradeAcceptPerOffer(t *testing.T) {
	rec := &decideRecorder{}
	e, _ := decideExt(t, rec)
	e.decideRun(context.Background(), "ext:sleeper:"+testLeague+":trade:860317606")
	want := []string{"true", "false", "false"} // send, decline, counter
	wantBy := []string{"me", "partner", "me"}  // You, the partner, the analyst's counter
	if len(rec.reqs) != len(want) {
		t.Fatalf("trade asked %d decisions, want %d", len(rec.reqs), len(want))
	}
	for i, req := range rec.reqs {
		st := stateMap(t, req)
		if req.Point != "trade_accept" || req.Baseline != want[i] || st["offer_index"] != float64(i) || st["partner_id"] != "860317606291283968" {
			t.Errorf("offer %d = %s %q index %v partner %v", i, req.Point, req.Baseline, st["offer_index"], st["partner_id"])
		}
		if st["offered_by"] != wantBy[i] {
			t.Errorf("offer %d offered_by = %v, want %s", i, st["offered_by"], wantBy[i])
		}
		if len(st["give"].([]any)) == 0 || len(st["get"].([]any)) == 0 {
			t.Errorf("offer %d state lacks give/get rows", i)
		}
		noAnswerKeys(t, req, "verdict", "my_roster", "partner_roster")
	}
}

// TestTradeGiveIsAlwaysMine: an offer written from the offering partner's side (give on their
// roster) reaches the state flipped; one already from my side is left alone.
func TestTradeGiveIsAlwaysMine(t *testing.T) {
	var a tradeArtifact
	if err := json.Unmarshal(fixtureBytes["trade"], &a); err != nil {
		t.Fatal(err)
	}
	mine := a.Offers[1]
	legacy := mine
	legacy.Give, legacy.Get = mine.Get, mine.Give
	a.Offers = []tradeOffer{mine, legacy}
	base := func(p artPlayer) playerRow { return playerRow{ID: p.ID, Name: p.Name} }
	ids := func(rows []playerRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	for i, req := range tradeRequests(base, decisionKeys{}, a) {
		st := req.State.(tradeState)
		if ids(st.Give) != "7021,11586" || ids(st.Get) != "9481" || st.By != "partner" {
			t.Errorf("offer %d give/get/by = %s/%s/%s, want 7021,11586/9481/partner", i, ids(st.Give), ids(st.Get), st.By)
		}
	}
}

// TestStatesCarryNoProse: no analyst text (why, delta, notes, summaries, the drop's free text)
// reaches any state, as a key or as a substring; only player facts and the proposal do.
func TestStatesCarryNoProse(t *testing.T) {
	rec := &decideRecorder{}
	e, fh := decideExt(t, rec)
	var w map[string]any
	if err := json.Unmarshal(fixtureBytes["waivers"], &w); err != nil {
		t.Fatal(err)
	}
	w["candidates"].([]any)[0].(map[string]any)["drop"] = "Rico Dowdle (Q) - buried behind Hubbard, drop on depth rationale"
	waivers, _ := json.Marshal(w)
	fh.artifacts["ext:sleeper:"+testLeague+":3:waivers"]["waivers"] = waivers

	proseKeys := strings.Fields("why delta drop note summary plan text source_note")
	var prose []string
	var collect func(v any)
	collect = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				switch s, ok := x.(string); {
				case ok && slices.Contains(proseKeys, k) && len(s) > 12:
					prose = append(prose, s)
				default:
					collect(x)
				}
			}
		case []any:
			for _, x := range v {
				collect(x)
			}
		}
	}
	for _, raw := range [][]byte{fixtureBytes["lineup"], waivers, fixtureBytes["trade"]} {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		collect(v)
	}
	if len(prose) < 10 {
		t.Fatalf("collected %d prose strings from the fixtures, want the whys, deltas and drops", len(prose))
	}

	for _, chat := range []string{":3:lineup", ":3:waivers", ":trade:860317606"} {
		e.decideRun(context.Background(), "ext:sleeper:"+testLeague+chat)
	}
	if len(rec.reqs) < 10 {
		t.Fatalf("asked %d decisions across lineup, waivers and trade", len(rec.reqs))
	}
	for _, req := range rec.reqs {
		noAnswerKeys(t, req, "reasoning", "why", "delta", "note", "summary", "verdict", "rank", "confidence", "drop_id")
		b, _ := json.Marshal(req.State)
		for _, p := range prose {
			if strings.Contains(string(b), p) {
				t.Errorf("%s state carries analyst text %q", req.Point, p)
			}
		}
	}
}

func TestOtherJobsAskNothing(t *testing.T) {
	rec := &decideRecorder{}
	e, _ := decideExt(t, rec)
	for _, id := range []string{"ext:sleeper:" + testLeague + ":3:digest", "ext:sleeper:" + testLeague + ":season-notes", "ext:github:x:1:lineup", "ext:sleeper:" + testLeague + ":4:lineup"} {
		e.decideRun(context.Background(), id)
	}
	if len(rec.reqs) != 0 {
		t.Errorf("asked %d decisions for non-decision chats", len(rec.reqs))
	}
}

func TestRunEndedObservesOnlyDoneRuns(t *testing.T) {
	rec := &decideRecorder{}
	e, _ := decideExt(t, rec)
	chat := "ext:sleeper:" + testLeague + ":trade:860317606"
	for _, s := range []sdk.RunStatus{sdk.RunFailed, sdk.RunCancelled, sdk.RunNeedsInput} {
		e.RunEnded(chat, sdk.RunOutcome{Status: s})
	}
	time.Sleep(100 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatalf("a run that did not finish asked %d decisions", rec.count())
	}
	e.RunEnded(chat, sdk.RunOutcome{Status: sdk.RunDone})
	waitFor(t, func() bool { return rec.count() == 3 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, req := range rec.reqs {
		if req.ChatID != chat {
			t.Errorf("%s ChatID = %q, want the run's chat %q", req.Point, req.ChatID, chat)
		}
	}
}

// TestDecideFailureChangesNothing: an erroring or absent handler leaves the
// artifact untouched, still asks every question, and RunEnded still clears.
func TestDecideFailureChangesNothing(t *testing.T) {
	rec := &decideRecorder{err: errors.New("handler down")}
	e, fh := decideExt(t, rec)
	chat := "ext:sleeper:" + testLeague + ":3:lineup"
	before := string(fh.artifacts[chat]["lineup"])
	e.markRunning(chat)
	e.RunEnded(chat, sdk.RunOutcome{Status: sdk.RunDone})
	if e.isRunning(chat) {
		t.Error("RunEnded left the chat running")
	}
	waitFor(t, func() bool { return rec.count() == 2 })
	if string(fh.artifacts[chat]["lineup"]) != before || len(fh.dispatched) != 0 {
		t.Error("a decision changed the artifact or dispatched a run")
	}

	nilExt, _ := decideExt(t, nil)
	nilExt.RunEnded(chat, sdk.RunOutcome{Status: sdk.RunDone}) // must not panic
}

func TestSlowDecideIsBounded(t *testing.T) {
	old := decideTimeout
	decideTimeout = 50 * time.Millisecond
	t.Cleanup(func() { decideTimeout = old })
	rec := &decideRecorder{wait: true}
	e, _ := decideExt(t, rec)
	start := time.Now()
	e.RunEnded("ext:sleeper:"+testLeague+":trade:860317606", sdk.RunOutcome{Status: sdk.RunDone})
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("RunEnded blocked %v on the handler", d)
	}
	waitFor(t, func() bool { return rec.count() == 3 })
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("three blocked calls took %v, want each cut at decideTimeout", d)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDecisionPointsDeclared(t *testing.T) {
	names := map[string]bool{}
	for _, p := range (&extension{}).DecisionPoints() {
		names[p.Name] = true
		if _, ok := p.Questions[p.Primary]; !ok || len(p.Modes) != 1 || p.Modes[0] != "observe" {
			t.Errorf("point %s: primary %q undeclared or modes %v not observe-only", p.Name, p.Primary, p.Modes)
		}
	}
	for _, n := range []string{"lineup_change", "waiver_pickup", "waiver_priority", "trade_accept"} {
		if !names[n] {
			t.Errorf("point %s not declared", n)
		}
	}
}

func TestRowsJoinOpponentAndRecentPoints(t *testing.T) {
	e, _ := decideExt(t, nil)
	ctx := context.Background()
	stats, err := e.client.WeekStats(ctx, "2026", 2)
	if err != nil || len(stats) == 0 {
		t.Fatalf("week-2 stats fixture: %v", err)
	}
	var id string
	for k, s := range stats {
		if _, ok := s["pts_ppr"]; ok {
			id = k
			break
		}
	}
	keys := decisionKeys{LeagueID: testLeague}
	row := e.joiner(ctx, &keys, 3)(artPlayer{ID: id, Name: "x"})
	if len(row.Recent) != 1 || row.Recent[0].Week != 2 || row.Recent[0].PtsPPR != stats[id]["pts_ppr"] {
		t.Errorf("week-3 row recent = %+v, want week 2's %v", row.Recent, stats[id]["pts_ppr"])
	}

	// Week 2 is the fixture's live week, so the slate joins an opponent.
	keys = decisionKeys{LeagueID: testLeague}
	row = e.joiner(ctx, &keys, 0)(artPlayer{ID: "7594", Name: "Chuba Hubbard", Pos: "RB", Team: "CAR"})
	if keys.Week != 2 || row.Opp == "" || row.Opp == "CAR" {
		t.Errorf("live-week row: week %d opp %q, want week 2 and CAR's opponent", keys.Week, row.Opp)
	}

	// A stop-3 chat whose artifact names the live week 2 stays week 3, with no week-2 opponent.
	keys = decisionKeys{LeagueID: testLeague, Week: 3}
	row = e.joiner(ctx, &keys, 2)(artPlayer{ID: "7594", Name: "Chuba Hubbard", Pos: "RB", Team: "CAR"})
	if keys.Week != 3 || row.Opp != "" {
		t.Errorf("stop-3 row: week %d opp %q, want week 3 and no opponent", keys.Week, row.Opp)
	}
}
