package vetting

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// updateJudgeGolden regenerates testdata/prompts; the goldens must stay byte-identical.
var updateJudgeGolden = flag.Bool("update-golden", false, "rewrite the judge prompt golden files")

var judgeTodayLine = regexp.MustCompile(`Today is [^\n]*\.`)

func checkJudgeGolden(t *testing.T, name, got string) {
	t.Helper()
	got = judgeTodayLine.ReplaceAllString(got, "Today is <DATE>.")
	path := filepath.Join("testdata", "prompts", name)
	if *updateJudgeGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test -run Golden -update-golden)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s: judge prompt changed\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// mustJudgeBehaviour assembles the judge behaviour layer or fails the test.
func mustJudgeBehaviour(t *testing.T, readTools, skills bool) string {
	t.Helper()
	p, err := resolveJudgePrompt(context.Background(), nil)
	if err != nil {
		t.Fatalf("judge prompt: %v", err)
	}
	return p.behaviour(readTools, skills)
}

// TestGoldenJudgePrompt pins the judge's assembled prompt for every
// tool-presence combination the clause selection branches on.
func TestGoldenJudgePrompt(t *testing.T) {
	for _, c := range []struct {
		name             string
		readTools, skill bool
	}{
		{"judge.notools", false, false},
		{"judge.readtools", true, false},
		{"judge.readtools.skills", true, true},
		{"judge.skills", false, true},
	} {
		checkJudgeGolden(t, c.name+".txt", promptbuilder.Judge(mustJudgeBehaviour(t, c.readTools, c.skill)))
	}
}

// Every judge's system prompt opens with the same static block, whatever tools it holds: a tool clause
// ahead of it ended the shared prefix (and the server's prompt cache) about 670 chars in.
func TestJudgePromptStaticPrefixSharedAcrossTools(t *testing.T) {
	p, err := resolveJudgePrompt(context.Background(), nil)
	if err != nil {
		t.Fatalf("judge prompt: %v", err)
	}
	static := strings.Join([]string{p.blocks["head"], p.blocks["artifact_tools"], p.blocks["tail"]}, " ")
	full := promptbuilder.Judge(p.behaviour(true, true))
	i := strings.Index(full, static)
	if i < 0 {
		t.Fatal("head, artifact_tools and tail are not one contiguous block ahead of the tool clauses")
	}
	prefix := full[:i+len(static)]
	for _, c := range []struct{ readTools, skills bool }{{false, false}, {true, false}, {false, true}} {
		if got := promptbuilder.Judge(p.behaviour(c.readTools, c.skills)); !strings.HasPrefix(got, prefix) {
			t.Errorf("readTools=%v skills=%v: prompt diverges from the static prefix at char %d", c.readTools, c.skills, commonPrefixLen(got, prefix))
		}
	}
}
