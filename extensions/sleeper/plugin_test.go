package sleeper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// quackBuiltinTools are the quack-native tools the plugin's agents may name,
// each a key of quack's internal/tools/registry.go builtin map; add one only after checking it is there.
var quackBuiltinTools = map[string]bool{
	"current_date": true, "grep_artifacts": true, "summarize": true,
	"weather": true, "web_fetch": true, "web_search": true,
}

type pluginAgent struct {
	name   string
	dir    string
	Tools  []string `yaml:"tools"`
	Skills []string `yaml:"skills"`
}

func loadPluginAgents(t *testing.T) []pluginAgent {
	t.Helper()
	dirs, err := filepath.Glob("plugin/agents/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no plugin agents found: %v", err)
	}
	var out []pluginAgent
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		a := pluginAgent{name: filepath.Base(dir), dir: dir}
		if err := yaml.Unmarshal(raw, &a); err != nil {
			t.Fatalf("%s: agent.yaml: %v", a.name, err)
		}
		out = append(out, a)
	}
	return out
}

// TestPluginAgentToolsExist: quack drops a plugin agent naming a tool it cannot
// build, so every tool must be this extension's own or a quack builtin.
func TestPluginAgentToolsExist(t *testing.T) {
	registered := map[string]bool{}
	for _, tl := range testExtension(t).Tools() {
		registered[tl.Name()] = true
	}
	for _, a := range loadPluginAgents(t) {
		if len(a.Tools) == 0 {
			t.Errorf("%s: agent.yaml lists no tools", a.name)
		}
		for _, name := range a.Tools {
			if !registered[name] && !quackBuiltinTools[name] {
				t.Errorf("%s: tool %q is neither a sleeper extension tool nor an allowlisted quack builtin", a.name, name)
			}
		}
	}
}

// TestGameToolsComeWithSleeperPlayer: only sleeper_player's description carries the game, lock and
// time rules the other game-returning tools point at, so an agent offered one must be offered it too.
func TestGameToolsComeWithSleeperPlayer(t *testing.T) {
	for _, a := range loadPluginAgents(t) {
		gameTool := slices.ContainsFunc(a.Tools, func(n string) bool {
			return n == "sleeper_roster" || n == "sleeper_matchup" || n == "sleeper_schedule"
		})
		if gameTool && !slices.Contains(a.Tools, "sleeper_player") {
			t.Errorf("%s: offers a game-returning tool without sleeper_player, which documents its game fields", a.name)
		}
	}
}

// TestPluginAgentsShipRubricSkillAndSchema: each judged agent has a prompt, a rubric,
// a sleeper: skill on disk, and a card artifact kind this extension declares a schema for.
func TestPluginAgentsShipRubricSkillAndSchema(t *testing.T) {
	schemas := testExtension(t).ArtifactSchemas()
	for _, a := range loadPluginAgents(t) {
		for _, f := range []string{"prompt.md", "rubric.yaml"} {
			if _, err := os.Stat(filepath.Join(a.dir, f)); err != nil {
				t.Errorf("%s: no %s", a.name, f)
			}
		}
		own := 0
		for _, s := range a.Skills {
			name, ok := strings.CutPrefix(s, "sleeper:")
			if !ok {
				continue
			}
			own++
			if _, err := os.Stat(filepath.Join("plugin/skills", name, "SKILL.md")); err != nil {
				t.Errorf("%s: skill %q has no SKILL.md in the plugin", a.name, s)
			}
		}
		if own == 0 {
			t.Errorf("%s: agent.yaml lists no sleeper: skill", a.name)
		}
		raw, err := os.ReadFile(filepath.Join(a.dir, "agent-card.json"))
		if err != nil {
			t.Fatal(err)
		}
		var card struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatalf("%s: agent-card.json: %v", a.name, err)
		}
		if _, ok := schemas[card.Artifact]; !ok {
			t.Errorf("%s: card artifact %q has no schema in ArtifactSchemas", a.name, card.Artifact)
		}
	}
}

// skillSchemaKinds pairs each skill's schema reference, which the agent reads,
// with the ui/schemas kind ArtifactSchemas validates its write against.
var skillSchemaKinds = map[string]string{
	"plugin/skills/draft-strategy/references/output-schema.json":                "draft",
	"plugin/skills/history-grading/references/output-schema.json":               "history",
	"plugin/skills/injury-and-news/references/output-schema.json":               "trends",
	"plugin/skills/injury-and-news/references/season-notes-schema.json":         "season-notes",
	"plugin/skills/matchup-recap/references/output-schema-digest.json":          "digest",
	"plugin/skills/matchup-recap/references/output-schema-retro.json":           "retro",
	"plugin/skills/start-sit/references/output-schema.json":                     "lineup",
	"plugin/skills/trade-evaluation/references/output-schema-trade.json":        "trade",
	"plugin/skills/trade-evaluation/references/output-schema-trade-finder.json": "trade-finder",
	"plugin/skills/waivers/references/output-schema.json":                       "waivers",
}

// TestSkillSchemasMatchArtifactSchemas: an agent told a looser schema than the
// one quack enforces writes artifacts that fail validation.
func TestSkillSchemasMatchArtifactSchemas(t *testing.T) {
	schemas := testExtension(t).ArtifactSchemas()
	refs, err := filepath.Glob("plugin/skills/*/references/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		kind, ok := skillSchemaKinds[ref]
		if !ok {
			t.Errorf("%s: no ui/schemas kind paired in skillSchemaKinds", ref)
			continue
		}
		b, err := os.ReadFile(ref)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, schemas[kind]) {
			t.Errorf("%s differs from ui/schemas/%s.json; copy the ui schema over it", ref, kind)
		}
	}
	if len(refs) != len(skillSchemaKinds) {
		t.Errorf("found %d schema references, skillSchemaKinds pairs %d", len(refs), len(skillSchemaKinds))
	}
}

// TestArtifactSchemasAcceptProdRejections: payloads prod refused (2026-10-13). A prose cap must leave
// room for inline link URLs the prompts exclude from their length, and a news item may name a player
// Sleeper's data does not resolve.
func TestArtifactSchemasAcceptProdRejections(t *testing.T) {
	why := "Burrow's CIN bye leaves your only active QB slot blank and Love is the top available QB - healthy, depth-chart 1, 367 yd/2 TD [in Week 5](https://www.pff.com/news/nfl-jordan-love-week-5-pff-passing-grade), plays at DAL (QB's 6th-ranked matchup). Claim now: 8th of 10, but only ~2 teams ahead are QB-starved. Spending the slot is the mandatory top-5-at-position bye-coverage case; if it doesn't land you enter future weeks last in line for any RB1/TE1 vacancy."
	note := "Stop 6 has no prior daily snapshot, so Sleeper field deltas are unavailable; items come from web sources fetched/searched this session. Sleeper fields win on conflict (Hendrickson/Lane still Out/IR in Sleeper despite optimistic coach quotes). Conner/Penix items carry no player object - not resolved against league rosters."
	cases := map[string]any{
		"waivers": map[string]any{"week": 6, "candidates": []any{map[string]any{
			"player": map[string]any{"id": "6804", "name": "Jordan Love"}, "proj": 21.77, "why": why}}},
		"trends": map[string]any{"source_note": note, "items": []any{map[string]any{
			"time": "2026-10-13", "source": "CBS Sports injury report", "kind": "injury",
			"player": map[string]any{"id": nil, "name": "James Conner", "pos": "RB", "team": "ARI", "inj": "IR"},
			"text":   "James Conner (foot) on IR, expected return Week 7.", "link": "https://www.cbssports.com/nfl/injuries/"}}},
	}
	schemas := testExtension(t).ArtifactSchemas()
	for kind, doc := range cases {
		var s jsonschema.Schema
		if err := json.Unmarshal(schemas[kind], &s); err != nil {
			t.Fatal(err)
		}
		rs, err := s.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(doc)
		var instance any
		_ = json.Unmarshal(b, &instance)
		if err := rs.Validate(instance); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}
