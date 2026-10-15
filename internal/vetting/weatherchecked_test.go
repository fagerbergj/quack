package vetting

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A sleeper_matchup-shaped reply: Soldier Field and Lambeau are open and outdoor, SoFi is a dome, and
// the locked game has already kicked off.
const weatherMatchupReply = `{"me":{"starters":[
 {"player_id":"chi","name":"Bears RB","game":{"game_id":"g1","kickoff":"2026-11-01T18:00:00Z","locked":false,"roof":"outdoor","venue":"Soldier Field","latitude":41.86,"longitude":-87.62}},
 {"player_id":"gb","name":"Packers WR","game":{"game_id":"g2","kickoff":"2026-11-01T18:00:00Z","locked":false,"roof":"outdoor","venue":"Lambeau Field","latitude":44.5,"longitude":-88.06}},
 {"player_id":"lac","name":"Chargers WR","game":{"game_id":"g3","kickoff":"2026-11-01T21:05:00Z","locked":false,"roof":"dome","venue":"SoFi Stadium","latitude":33.95,"longitude":-118.34}},
 {"player_id":"thu","name":"Thursday TE","game":{"game_id":"g4","kickoff":"2026-10-29T00:15:00Z","locked":true,"roof":"outdoor","venue":"Arrowhead Stadium","latitude":39.05,"longitude":-94.48}}
]}}`

func weatherCfg(lineup string) Config {
	return Config{
		RubricSpecs:  map[string]criterionSpec{weatherCheckedCriterion: {Name: weatherCheckedCriterion, Deterministic: true}},
		RecordReader: fakeLoader{"lineup:chat": lineup},
	}
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func weatherCall(id string, lat, lon float64, resp map[string]any) []evtPart {
	args := map[string]any{"latitude": lat, "longitude": lon, "kickoff": "2026-11-01T18:00:00Z"}
	return []evtPart{fnCall(id, "weather", args), fnResp(id, "weather", resp)}
}

func TestWeatherCheckedCriterion(t *testing.T) {
	lineup := `{"week":8,"team":"T","starters":[
	 {"slot":"RB","player":{"id":"chi","name":"Bears RB"},"close":true},
	 {"slot":"WR","player":{"id":"lac","name":"Chargers WR"},"close":true},
	 {"slot":"TE","player":{"id":"thu","name":"Thursday TE"},"close":true}],
	 "bench":[{"player":{"id":"gb","name":"Packers WR"}}]}`
	ok := map[string]any{"latitude": 41.86, "hourly": []any{}}
	failed := map[string]any{"error": "weather: open-meteo: timeout"}
	tests := []struct {
		name      string
		lineup    string
		calls     []evtPart
		answer    string
		want      float64
		wantInWhy string
	}{
		{"outdoor close call checked; dome and locked games exempt", lineup, weatherCall("w1", 41.86, -87.62, ok), "", 1, "1 of 1"},
		{"call for another venue leaves it unchecked", lineup, weatherCall("w1", 44.5, -88.06, ok), "", 0, "unchecked: Soldier Field"},
		{"errored call the answer acknowledges", lineup, weatherCall("w1", 41.86, -87.62, failed), "The weather forecast was unavailable.", 1, "1 of 1"},
		{"errored call never mentioned", lineup, weatherCall("w1", 41.86, -87.62, failed), "", 0, "unchecked: Soldier Field"},
		{"bench player in an outdoor close call", strings.Replace(lineup, `"Packers WR"}}`, `"Packers WR"},"close":true}`, 1),
			weatherCall("w1", 41.86, -87.62, ok), "", 0.5, "unchecked: Lambeau Field"},
		{"only dome and locked close calls", strings.Replace(lineup, `"Bears RB"},"close":true`, `"Bears RB"}`, 1), nil, "", 1, "nothing to check"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parts := append([]evtPart{
				fnCall("m", "sleeper_matchup", map[string]any{}),
				fnResp("m", "sleeper_matchup", decodeJSON(t, weatherMatchupReply)),
				fnCall("a", "write_artifact", map[string]any{"kind": "lineup"}),
				fnResp("a", "write_artifact", map[string]any{"result": "ok: id=lineup:chat revision=1"}),
			}, tc.calls...)
			act := activityFromSessionAt(newTestSession(t, parts...), "", "")
			cfg := weatherCfg(tc.lineup)
			got, applies := weatherCheckedCriterionScore(context.Background(), deliverableText(context.Background(), tc.answer, act, cfg.RecordReader), act, cfg)
			if !applies || got.Score != tc.want || !strings.Contains(got.Reason, tc.wantInWhy) {
				t.Errorf("got %v %+v, want score %v with %q", applies, got, tc.want, tc.wantInWhy)
			}
		})
	}
}

func TestWeatherCheckedCriterionAbsent(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("a", "write_artifact", map[string]any{"kind": "lineup"}),
		fnResp("a", "write_artifact", map[string]any{"result": "ok: id=lineup:chat revision=1"}),
	), "", "")
	if _, ok := weatherCheckedCriterionScore(context.Background(), "", act, Config{RecordReader: fakeLoader{"lineup:chat": `{"starters":[{}]}`}}); ok {
		t.Error("scored for a rubric that does not declare it")
	}
	noStore := weatherCfg("")
	noStore.RecordReader = nil
	for _, cfg := range []Config{weatherCfg("not json"), noStore} {
		if _, ok := weatherCheckedCriterionScore(context.Background(), "", act, cfg); ok {
			t.Error("scored with no readable lineup artifact")
		}
	}
}
