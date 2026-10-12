package sleeper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
	"gopkg.in/yaml.v3"
)

// fixtureCase pairs a recorded request with the generated type its 200 decodes into and, when shape is
// set, the openapi.yaml schema whose required fields every instance must carry.
type fixtureCase struct {
	name   string
	url    string
	dst    func() any
	schema string // components.schemas name; "" skips the required-field check
	shape  string // "object" | "array" | "map" - how instances sit in the payload
}

// fixtureKey must match cmd/qa-mock's key function exactly.
func fixtureKey(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	key := "GET_" + u.Path
	if u.RawQuery != "" {
		key += "_" + u.RawQuery
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ".json", nil
}

const (
	testLeague = "1356407594683482112"
	testUser   = "860211057418018816"
	testDraft  = "1356407594700242944"
	pastLeague = "1257102049204514816"
	pastDraft  = "1257102049204514817"
)

func fixtureCases() []fixtureCase {
	return []fixtureCase{
		{"user", "/v1/user/" + testUser, func() any { return &sleepergen.User{} }, "User", "object"},
		{"user leagues", "/v1/user/" + testUser + "/leagues/nfl/2026", func() any { return &[]sleepergen.League{} }, "League", "array"},
		{"league", "/v1/league/" + testLeague, func() any { return &sleepergen.League{} }, "League", "object"},
		{"rosters", "/v1/league/" + testLeague + "/rosters", func() any { return &[]sleepergen.Roster{} }, "Roster", "array"},
		{"league users", "/v1/league/" + testLeague + "/users", func() any { return &[]sleepergen.LeagueUser{} }, "LeagueUser", "array"},
		{"matchups w2", "/v1/league/" + testLeague + "/matchups/2", func() any { return &[]sleepergen.Matchup{} }, "Matchup", "array"},
		{"matchups w1", "/v1/league/" + testLeague + "/matchups/1", func() any { return &[]sleepergen.Matchup{} }, "Matchup", "array"},
		{"transactions w2", "/v1/league/" + testLeague + "/transactions/2", func() any { return &[]sleepergen.Transaction{} }, "Transaction", "array"},
		{"transactions w1", "/v1/league/" + testLeague + "/transactions/1", func() any { return &[]sleepergen.Transaction{} }, "Transaction", "array"},
		{"winners bracket", "/v1/league/" + testLeague + "/winners_bracket", func() any { return &[]sleepergen.BracketMatch{} }, "BracketMatch", "array"},
		{"losers bracket", "/v1/league/" + testLeague + "/losers_bracket", func() any { return &[]sleepergen.BracketMatch{} }, "BracketMatch", "array"},
		{"traded picks", "/v1/league/" + testLeague + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }, "TradedPick", "array"},
		{"league drafts", "/v1/league/" + testLeague + "/drafts", func() any { return &[]sleepergen.Draft{} }, "Draft", "array"},
		{"draft", "/v1/draft/" + testDraft, func() any { return &sleepergen.Draft{} }, "Draft", "object"},
		{"draft picks", "/v1/draft/" + testDraft + "/picks", func() any { return &[]sleepergen.DraftPick{} }, "DraftPick", "array"},
		{"draft traded picks", "/v1/draft/" + testDraft + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }, "TradedPick", "array"},
		{"state", "/v1/state/nfl", func() any { return &sleepergen.NflState{} }, "NflState", "object"},
		{"trending add", "/v1/players/nfl/trending/add?lookback_hours=24&limit=25", func() any { return &[]sleepergen.TrendingPlayer{} }, "TrendingPlayer", "array"},
		{"trending drop", "/v1/players/nfl/trending/drop?lookback_hours=24&limit=25", func() any { return &[]sleepergen.TrendingPlayer{} }, "TrendingPlayer", "array"},
		{"one player", "/v1/players/nfl/6797", func() any { return &sleepergen.Player{} }, "Player", "object"},
		{"players dump", "/v1/players/nfl", func() any { return &map[string]sleepergen.Player{} }, "Player", "map"},
		{"projections w2", "/v1/projections/nfl/regular/2026/2", func() any { return &map[string]sleepergen.StatMap{} }, "", ""},
		{"projections w1", "/v1/projections/nfl/regular/2026/1", func() any { return &map[string]sleepergen.StatMap{} }, "", ""},
		{"stats w2", "/v1/stats/nfl/regular/2026/2", func() any { return &map[string]sleepergen.StatMap{} }, "", ""},
		{"stats w1", "/v1/stats/nfl/regular/2026/1", func() any { return &map[string]sleepergen.StatMap{} }, "", ""},
		{"depth chart", "/players/nfl/TB/depth_chart", func() any { return &map[string][]string{} }, "", ""},
		{"schedule", "/schedule/nfl/regular/2026", func() any { return &[]sleepergen.Game{} }, "Game", "array"},
		{"scores w2", "/scores/nfl/regular/2026/2", func() any { return &[]sleepergen.GameScore{} }, "GameScore", "array"},
		{"research", "/players/nfl/research/regular/2026/2", func() any { return &map[string]sleepergen.ResearchEntry{} }, "", ""},
		{"player season stats", "/stats/nfl/player/6797?season_type=regular&season=2026", func() any { return &sleepergen.PlayerStatEntry{} }, "PlayerStatEntry", "object"},
		{"player game log", "/stats/nfl/player/6797?grouping=week&season=2026&season_type=regular", func() any { return &map[string]*sleepergen.PlayerStatEntry{} }, "PlayerStatEntry", "map"},
		{"past league", "/v1/league/" + pastLeague, func() any { return &sleepergen.League{} }, "League", "object"},
		{"past rosters", "/v1/league/" + pastLeague + "/rosters", func() any { return &[]sleepergen.Roster{} }, "Roster", "array"},
		{"past winners bracket", "/v1/league/" + pastLeague + "/winners_bracket", func() any { return &[]sleepergen.BracketMatch{} }, "BracketMatch", "array"},
		{"past matchups w1", "/v1/league/" + pastLeague + "/matchups/1", func() any { return &[]sleepergen.Matchup{} }, "Matchup", "array"},
		{"past matchups w14", "/v1/league/" + pastLeague + "/matchups/14", func() any { return &[]sleepergen.Matchup{} }, "Matchup", "array"},
		{"past transactions w1", "/v1/league/" + pastLeague + "/transactions/1", func() any { return &[]sleepergen.Transaction{} }, "Transaction", "array"},
		{"past transactions w5 (trade)", "/v1/league/" + pastLeague + "/transactions/5", func() any { return &[]sleepergen.Transaction{} }, "Transaction", "array"},
		{"past draft", "/v1/draft/" + pastDraft, func() any { return &sleepergen.Draft{} }, "Draft", "object"},
		{"past draft picks", "/v1/draft/" + pastDraft + "/picks", func() any { return &[]sleepergen.DraftPick{} }, "DraftPick", "array"},
		{"past draft traded picks", "/v1/draft/" + pastDraft + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }, "TradedPick", "array"},
		{"season stats 2025", "/v1/stats/nfl/regular/2025", func() any { return &map[string]sleepergen.StatMap{} }, "", ""},
	}
}

// TestFixturesMatchGeneratedTypes decodes every fixture with unknown fields disallowed and checks
// openapi.yaml's required fields are present, since a missing field just decodes as zero.
func TestFixturesMatchGeneratedTypes(t *testing.T) {
	required := loadRequiredFields(t)
	for _, tc := range fixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			data := readFixture(t, tc.url)
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(tc.dst()); err != nil {
				t.Fatalf("decode into generated type: %v", err)
			}
			checkRequiredFields(t, data, tc.schema, tc.shape, required[tc.schema])
		})
	}
}

func readFixture(t *testing.T, rawURL string) []byte {
	t.Helper()
	key, err := fixtureKey(rawURL)
	if err != nil {
		t.Fatalf("fixtureKey: %v", err)
	}
	path := filepath.Join("testdata", "get", key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s (for %s): %v", path, rawURL, err)
	}
	return data
}

// openAPISchemas is each openapi.yaml schema's required-field list.
type openAPISchemas struct {
	Components struct {
		Schemas map[string]struct {
			Required []string `yaml:"required"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

// loadRequiredFields reads required fields from openapi.yaml itself so the spec stays the source of truth.
func loadRequiredFields(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var spec openAPISchemas
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	out := make(map[string][]string, len(spec.Components.Schemas))
	for name, s := range spec.Components.Schemas {
		out[name] = s.Required
	}
	return out
}

// checkRequiredFields asserts every required field is present on each object in raw, per shape
// (object, array elements, or map values), independent of the generated Go type.
func checkRequiredFields(t *testing.T, raw []byte, schema, shape string, required []string) {
	t.Helper()
	missing, err := missingRequiredFields(raw, schema, shape, required)
	if err != nil {
		t.Fatalf("check required fields: %v", err)
	}
	for _, m := range missing {
		t.Error(m)
	}
}

// missingRequiredFields is checkRequiredFields without *testing.T, so the mutation test can assert on it.
func missingRequiredFields(raw []byte, schema, shape string, required []string) ([]string, error) {
	if schema == "" || len(required) == 0 {
		return nil, nil
	}
	instances, err := jsonInstances(raw, shape)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, obj := range instances {
		for _, field := range required {
			if _, ok := obj[field]; !ok {
				out = append(out, fmt.Sprintf("instance %d: missing required field %q (schema %s)", i, field, schema))
			}
		}
	}
	return out, nil
}

// jsonInstances unmarshals raw per shape into a slice of top-level objects.
func jsonInstances(raw []byte, shape string) ([]map[string]any, error) {
	switch shape {
	case "object":
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		return []map[string]any{obj}, nil
	case "array":
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	case "map":
		var m map[string]map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(m))
		for _, v := range m {
			if v != nil { // a keyed-by-week map carries null for unplayed weeks
				out = append(out, v)
			}
		}
		return out, nil
	default:
		return nil, nil
	}
}

// TestAllFixturesCovered catches a recorded fixture no case decodes, which would rot unnoticed.
func TestAllFixturesCovered(t *testing.T) {
	want := map[string]bool{}
	for _, tc := range fixtureCases() {
		key, err := fixtureKey(tc.url)
		if err != nil {
			t.Fatalf("fixtureKey: %v", err)
		}
		want[key] = true
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "get"))
	if err != nil {
		t.Fatalf("read testdata/get: %v", err)
	}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("fixture %s has no contract-test case covering it", e.Name())
		}
	}
}

// TestMutationDroppedRequiredFieldFails proves checkRequiredFields catches a dropped field, which
// DisallowUnknownFields alone would not.
func TestMutationDroppedRequiredFieldFails(t *testing.T) {
	data := readFixture(t, "/v1/state/nfl")
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal state fixture: %v", err)
	}
	delete(obj, "week")
	mutated, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("remarshal mutated state fixture: %v", err)
	}
	required := loadRequiredFields(t)["NflState"]
	missing, err := missingRequiredFields(mutated, "NflState", "object", required)
	if err != nil {
		t.Fatalf("missingRequiredFields: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("expected the required-field check to flag a fixture missing \"week\", but it found nothing")
	}
}
