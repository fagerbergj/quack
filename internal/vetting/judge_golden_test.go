package vetting

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// updateJudgeGolden regenerates testdata/prompts. The goldens were captured
// before the artifact-resolver change (#1420) and must stay byte-identical.
var updateJudgeGolden = flag.Bool("update-golden", false, "rewrite the judge prompt golden files")

var judgeTodayLine = regexp.MustCompile(`Today is \d{4}-\d{2}-\d{2}\.`)

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
	b, err := judgeBehaviour(readTools, skills)
	if err != nil {
		t.Fatalf("judge behaviour: %v", err)
	}
	return b
}

// TestGoldenJudgePrompt pins the judge's assembled prompt for every
// tool-presence combination judgeBehaviour branches on.
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
		checkJudgeGolden(t, c.name+".txt", promptbuilder.Judge(nil, mustJudgeBehaviour(t, c.readTools, c.skill)))
	}
}
