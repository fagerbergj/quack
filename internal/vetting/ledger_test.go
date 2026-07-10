package vetting

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// questionContent wraps text as a user content for prompt-builder tests.
func questionContent(text string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
}

func TestRecordWsOpSummaries(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		resp map[string]any
		want []string // substrings the detail must contain
	}{
		{
			tool: "git_commit",
			args: map[string]any{"dir": "repo", "message": "fix: the bug"},
			resp: map[string]any{"sha": "abc123", "files_changed": float64(2)},
			want: []string{"git_commit(", `dir="repo"`, `message="fix: the bug"`, `sha="abc123"`, "files_changed=2"},
		},
		{
			tool: "git_clone",
			args: map[string]any{"url": "https://github.com/x/y", "dir": "y"},
			resp: map[string]any{"dir": "y", "head": "def456", "default_branch": "main"},
			want: []string{"git_clone(", `url="https://github.com/x/y"`, `head="def456"`},
		},
		{
			tool: "git_branch",
			args: map[string]any{"dir": "repo", "name": "feat/x"},
			resp: map[string]any{"current": "feat/x"},
			want: []string{"git_branch(", `name="feat/x"`, `current="feat/x"`},
		},
		{
			tool: "run_command",
			args: map[string]any{"dir": "repo", "command": "go test ./..."},
			resp: map[string]any{"exit_code": float64(0), "output": "ok"},
			want: []string{"run_command(", `command="go test ./..."`, "exit_code=0"},
		},
		{
			tool: "write_file",
			args: map[string]any{"path": "CONTRIBUTING.md"},
			resp: map[string]any{"bytes": float64(120), "created": true},
			want: []string{"write_file(", `path="CONTRIBUTING.md"`, "bytes=120", "created=true"},
		},
		{
			tool: "edit_file",
			args: map[string]any{"path": "main.go"},
			resp: map[string]any{"replacements": float64(1)},
			want: []string{"edit_file(", "replacements=1"},
		},
	}
	for _, c := range cases {
		op := recordWsOp(c.tool, c.args, c.resp)
		for _, w := range c.want {
			if !strings.Contains(op.detail, w) {
				t.Errorf("%s detail = %q, want it to contain %q", c.tool, op.detail, w)
			}
		}
	}
}

func TestRecordWsOpFailureIsRecorded(t *testing.T) {
	op := recordWsOp("run_command", map[string]any{"dir": "repo", "command": "go test ./..."},
		map[string]any{"error": "run_command: timed out after 60s"})
	if !strings.Contains(op.detail, "FAILED") || !strings.Contains(op.detail, "timed out") {
		t.Errorf("detail = %q, want a FAILED marker with the error", op.detail)
	}
}

func TestRecordWsOpReadFileKeepsSample(t *testing.T) {
	op := recordWsOp("read_file", map[string]any{"path": "README.md"},
		map[string]any{"content": "# Real README\nreal first line", "total_lines": float64(2)})
	if !strings.Contains(op.sample, "Real README") {
		t.Errorf("sample = %q, want the file content head", op.sample)
	}
	long := strings.Repeat("x", 5000)
	op = recordWsOp("read_file", map[string]any{"path": "big.txt"}, map[string]any{"content": long})
	if len(op.sample) > fetchSampleBytes {
		t.Errorf("sample len = %d, want ≤ %d (trimToSample)", len(op.sample), fetchSampleBytes)
	}
}

func TestBuildWorkspaceSectionRendersLedger(t *testing.T) {
	act := workerActivity{workspace: []wsOp{
		{tool: "read_file", detail: `read_file(path="README.md")`, sample: "# Real README"},
		{tool: "git_commit", detail: `git_commit(dir="repo", message="fix") → sha="abc123", files_changed=1`},
	}}
	got := buildWorkspaceSection(act)
	for _, w := range []string{
		"Workspace activity",
		"do not contradict this",
		"NOT listed here did not happen",
		`read_file(path="README.md")`,
		"content sample: # Real README",
		`sha="abc123"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("workspace section missing %q:\n%s", w, got)
		}
	}
}

func TestBuildWorkspaceSectionEmptyForNoOps(t *testing.T) {
	if got := buildWorkspaceSection(workerActivity{}); got != "" {
		t.Errorf("workspace section for no ops = %q, want empty (web nodes unchanged)", got)
	}
}

func TestBuildWorkspaceSectionCapsAtTail(t *testing.T) {
	var ops []wsOp
	for i := 0; i < maxLedgerOps+10; i++ {
		ops = append(ops, wsOp{tool: "read_file", detail: fmt.Sprintf("read_file(path=\"f%d\")", i)})
	}
	got := buildWorkspaceSection(workerActivity{workspace: ops})
	if !strings.Contains(got, "10 earlier operation(s) omitted") {
		t.Errorf("expected an omission note, got:\n%s", got[:200])
	}
	if strings.Contains(got, `path="f0"`) {
		t.Error("earliest op should be omitted (tail kept)")
	}
	if !strings.Contains(got, fmt.Sprintf("f%d", maxLedgerOps+9)) {
		t.Error("latest op should be kept (tail)")
	}
}

func TestBuildActivitySectionIncludesWorkspace(t *testing.T) {
	act := workerActivity{
		workspace: []wsOp{{tool: "git_commit", detail: `git_commit(dir="repo") → sha="abc123"`}},
	}
	got := buildActivitySection(act)
	if !strings.Contains(got, "Workspace activity") || !strings.Contains(got, "abc123") {
		t.Errorf("activity section missing the workspace ledger:\n%s", got)
	}
}

func TestBuildJudgePromptCarriesLedger(t *testing.T) {
	act := workerActivity{workspace: []wsOp{
		{tool: "read_file", detail: `read_file(path="README.md")`, sample: "# Real README"},
	}}
	got := buildJudgePrompt("", "rubric text", questionContent("do the task"), "the answer", act)
	if !strings.Contains(got, "Workspace activity") || !strings.Contains(got, `read_file(path="README.md")`) {
		t.Errorf("judge prompt missing the workspace ledger:\n%s", got)
	}
	if !strings.Contains(got, "content sample: # Real README") {
		t.Errorf("judge prompt missing the read sample:\n%s", got)
	}
	// A web-research node (no workspace ops) leaves the judge prompt exactly
	// as before — no empty header.
	got = buildJudgePrompt("", "rubric text", questionContent("q"), "a", workerActivity{})
	if strings.Contains(got, "Workspace activity") {
		t.Errorf("judge prompt should carry no workspace section without ops:\n%s", got)
	}
}
