package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestLive_PiRound drives one REAL pi-acp round (tools/pi-acp/pi-acp.mjs
// driving pi) against a live OpenAI-compatible endpoint - the smoke harness
// for the integration, not a CI test. Run it by hand:
// QUACK_ACP_LIVE=1 QUACK_LLM_ENDPOINT=http://host:port/v1 QUACK_CODER_MODEL=qwen3-coder-next go test ./internal/acp/ -run TestLive_PiRound -v -timeout 10m
func TestLive_PiRound(t *testing.T) {
	if os.Getenv("QUACK_ACP_LIVE") == "" {
		t.Skip("live test: set QUACK_ACP_LIVE=1 (needs node, pi, and a live endpoint on PATH)")
	}
	endpoint, model := os.Getenv("QUACK_LLM_ENDPOINT"), os.Getenv("QUACK_CODER_MODEL")
	if endpoint == "" || model == "" {
		t.Fatal("live test: QUACK_LLM_ENDPOINT and QUACK_CODER_MODEL must be set")
	}
	type m = map[string]any
	cfg := m{
		"endpoint": endpoint,
		"api_key":  "unused",
		"model":    model,
	}
	// The same skills injection production uses (serve.acpSkillPaths): quack's
	// shipped skill library, discovered by pi's skill_paths glob.
	if skills, err := filepath.Abs("../../skills"); err == nil {
		cfg["skill_paths"] = []string{skills}
	}
	content, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	shim, err := filepath.Abs("../../tools/pi-acp/pi-acp.mjs")
	if err != nil {
		t.Fatal(err)
	}
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:      []string{"node", shim},
		Env:          []string{"PI_ACP_CONFIG=" + string(content)},
		Home:         t.TempDir(),
		Jail:         jail,
		UserID:       "live",
		StartTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	var specs []eventSpec
	err = a.round(ctx, cwd, "", workspace.Caps{},
		"Create a file named hello.txt in the current directory containing exactly the word: hi\nThen reply with a single line confirming what you did.", "", "", "", "",
		func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	for _, s := range specs {
		for _, p := range s.parts {
			switch {
			case p.FunctionCall != nil:
				t.Logf("tool: %s %v (partial=%v)", p.FunctionCall.Name, p.FunctionCall.Args, s.partial)
			case p.Thought:
				t.Logf("thought: %.80s", p.Text)
			}
		}
	}
	final := specs[len(specs)-1]
	t.Logf("answer: %s", final.parts[0].Text)
	got, err := os.ReadFile(filepath.Join(cwd, "hello.txt"))
	if err != nil {
		t.Fatalf("the agent did not create hello.txt: %v", err)
	}
	t.Logf("hello.txt: %q", got)
}
