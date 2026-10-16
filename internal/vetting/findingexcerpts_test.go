package vetting

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// numberedFile writes n lines "line 1".."line n" to dir/name.
func numberedFile(t *testing.T, dir, name string, n int) {
	t.Helper()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, name), sb.String())
}

func TestSourceExcerptsSection(t *testing.T) {
	dir := t.TempDir()
	numberedFile(t, dir, "pkg/a.go", 30)
	anchor := func(path string, line int) StagedDelivery {
		return StagedDelivery{Comments: []ReviewComment{{Path: path, Line: line, Body: "blocking: x"}}}
	}
	for _, tc := range []struct {
		name      string
		sd        StagedDelivery
		want      []string
		notWant   []string
		wantCount int
	}{
		{"single line shows 6 either side, cited line marked", anchor("pkg/a.go", 10),
			[]string{"pkg/a.go:10 (lines 4-16 of 30)", ">    10  line 10", "      4  line 4", "     16  line 16"},
			[]string{"line 3\n", "line 17\n"}, 1},
		{"range marks every cited line", StagedDelivery{Verified: []string{"checked pkg/a.go:12-14 handles nil"}},
			[]string{"pkg/a.go:12-14 (lines 6-20 of 30)", ">    12  line 12", ">    14  line 14", "     15  line 15"}, nil, 1},
		{"line past the end is a note", anchor("pkg/a.go", 500),
			[]string{"pkg/a.go:500 - cited line 500 is past the end of the file (30 lines)"}, nil, 1},
		{"missing anchored file is a note", anchor("pkg/nope.go", 3),
			[]string{"pkg/nope.go:3 - file not found in the reviewed tree"}, nil, 1},
		{"escaping path is refused", anchor("../../etc/passwd", 1),
			[]string{"file not found in the reviewed tree"}, []string{"root:"}, 1},
		{"missing prose cite is skipped", StagedDelivery{Verified: []string{"retry.go:13 has no method check"}},
			nil, nil, 0},
		{"repeated cites render once", StagedDelivery{
			Comments: []ReviewComment{{Path: "pkg/a.go", Line: 5, Body: "see pkg/a.go:5"}},
			Verified: []string{"./pkg/a.go:5 read"}},
			[]string{"pkg/a.go:5 (lines 1-11 of 30)"}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := sourceExcerptsSection(dir, reviewCites(tc.sd, dir))
			if n != tc.wantCount {
				t.Fatalf("count = %d, want %d\n%s", n, tc.wantCount, got)
			}
			if strings.Count(got, "pkg/a.go:5 (") > 1 {
				t.Errorf("repeated cite rendered twice:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
		})
	}
}

// Many cites stop at excerptMaxCount, a few huge ranges stop at excerptBudget; both say what was left out.
func TestSourceExcerptsSection_Budget(t *testing.T) {
	dir := t.TempDir()
	numberedFile(t, dir, "a.go", 2000)
	var cites []sourceCite
	for i := 1; i <= 30; i++ {
		cites = append(cites, sourceCite{path: "a.go", start: i * 50, end: i * 50, anchored: true})
	}
	got, n := sourceExcerptsSection(dir, cites)
	if n != excerptMaxCount || !strings.Contains(got, "(10 more cites not excerpted") {
		t.Fatalf("count = %d, want %d with 10 left out:\n%s", n, excerptMaxCount, got)
	}

	long := strings.Repeat("x", 1000)
	writeFile(t, filepath.Join(dir, "wide.go"), strings.Repeat(long+"\n", 400))
	cites = nil
	for i := 0; i < 10; i++ {
		cites = append(cites, sourceCite{path: "wide.go", start: 1 + i*40, end: 40 + i*40, anchored: true})
	}
	got, n = sourceExcerptsSection(dir, cites)
	if len(got) > excerptBudget+len(sourceExcerptsHeader)+100 || n == 0 || n == 10 {
		t.Fatalf("budget not applied: %d bytes, %d cites", len(got), n)
	}
	if !strings.Contains(got, "cited range clipped") && !strings.Contains(got, "…") {
		t.Errorf("long lines or ranges not clipped:\n%s", got[:500])
	}
}

// A code-review round's judge input carries the excerpts, and a pass that read only them is not re-judged.
func TestJudgeInput_ReviewRoundCarriesExcerpts(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = true
	cfg.Rubric = "score 0-10"
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Comments: []ReviewComment{{Path: "x.go", Line: 1, Body: "nit: name"}}},
	}}

	var calls int32
	var first string
	judge := fnLLM(func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			first = stubAllText(req)
		}
		return stubText(`{"score": 3, "passed": true, "feedback": ""}`), nil
	})
	factory := NewJudgeFactory(judge, []tool.Tool{newSpyReadTool(t, "package x", new(int32))}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review the pull request"}}}
	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "approved", act, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if !strings.Contains(first, "QUACK-FETCHED SOURCE EXCERPTS") || !strings.Contains(first, ">     1  package x") {
		t.Fatalf("judge input lacks the excerpt:\n%s", first)
	}
	if calls != 1 {
		t.Errorf("model calls = %d, want 1: the excerpt counts as a read", calls)
	}
}
