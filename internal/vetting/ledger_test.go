package vetting

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

// questionContent wraps text as a user content for prompt-builder tests.
func questionContent(text string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
}

// evtPart pairs a genai part with the role its event carries.
type evtPart struct {
	role string
	part *genai.Part
}

func fnCall(id, name string, args map[string]any) evtPart {
	return evtPart{role: "model", part: &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}}
}

func fnResp(id, name string, resp map[string]any) evtPart {
	return evtPart{role: "user", part: &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: resp}}}
}

// newTestSession builds an in-memory session carrying one event per part, in
// order — enough to exercise activityFromSession's event walk.
func newTestSession(t *testing.T, parts ...evtPart) session.Session {
	t.Helper()
	svc := session.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	sess := resp.Session
	for _, ep := range parts {
		ev := session.NewEvent(ctx, "test")
		ev.Author = "coder"
		ev.Content = &genai.Content{Role: ep.role, Parts: []*genai.Part{ep.part}}
		if err := svc.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatal(err)
		}
	}
	return sess
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
	got := buildJudgePrompt("", "rubric text", questionContent("do the task"), "the answer", "", act)
	if !strings.Contains(got, "Workspace activity") || !strings.Contains(got, `read_file(path="README.md")`) {
		t.Errorf("judge prompt missing the workspace ledger:\n%s", got)
	}
	if !strings.Contains(got, "content sample: # Real README") {
		t.Errorf("judge prompt missing the read sample:\n%s", got)
	}
	// A web-research node (no workspace ops) leaves the judge prompt exactly
	// as before — no empty header.
	got = buildJudgePrompt("", "rubric text", questionContent("q"), "a", "", workerActivity{})
	if strings.Contains(got, "Workspace activity") {
		t.Errorf("judge prompt should carry no workspace section without ops:\n%s", got)
	}
}

func TestBuildChangedFilesSectionReadsRealDisk(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := j.UserRoot("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "repo/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The worker wrote logic but NO page — the exact incomplete-deliverable the
	// judge must be able to see rather than trust the answer's "it's done".
	if err := os.WriteFile(filepath.Join(root, "repo/app/logic.ts"), []byte("export const GRAVITY = 0.5 // real on-disk content"), 0o644); err != nil {
		t.Fatal(err)
	}

	act := workerActivity{written: []string{"repo/app/logic.ts"}}
	got := buildChangedFilesSection(act, j, "u1", "")
	if !strings.Contains(got, "repo/app/logic.ts") || !strings.Contains(got, "real on-disk content") {
		t.Errorf("section missing the real file content:\n%s", got)
	}
	if !strings.Contains(got, "ACTUAL CURRENT CONTENT") {
		t.Errorf("section missing the header:\n%s", got)
	}
	// No jail / no written files → no section (pure-research + unjailed paths).
	if s := buildChangedFilesSection(act, nil, "u1", ""); s != "" {
		t.Errorf("nil jail should yield no section, got:\n%s", s)
	}
	if s := buildChangedFilesSection(workerActivity{}, j, "u1", ""); s != "" {
		t.Errorf("no written files should yield no section, got:\n%s", s)
	}
	// A path that no longer exists on disk is skipped, not an error.
	if s := buildChangedFilesSection(workerActivity{written: []string{"repo/gone.ts"}}, j, "u1", ""); s != "" {
		t.Errorf("unreadable path should be skipped, got:\n%s", s)
	}
}

// TestBuildChangedFilesSectionUsesPerChatScope pins the coupling with per-chat
// isolation: the worker writes into <root>/<user>/<chatID>/, so the judge must
// re-read from the SAME per-chat dir. Reading from the per-user root (chatID
// "") finds nothing — the exact silent no-op the chatID param prevents.
func TestBuildChangedFilesSectionUsesPerChatScope(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const chatID = "chat-42"
	// The worker's file lands under the per-chat scope, where its tools wrote it.
	chatRepo, err := j.Resolve("u1", chatID, "repo/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(chatRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatRepo, "logic.ts"), []byte("export const REAL = 1 // per-chat content"), 0o644); err != nil {
		t.Fatal(err)
	}
	act := workerActivity{written: []string{"repo/app/logic.ts"}}

	// With the chat scope, the judge sees the real per-chat file.
	if got := buildChangedFilesSection(act, j, "u1", chatID); !strings.Contains(got, "per-chat content") {
		t.Errorf("per-chat section missing the real file content:\n%s", got)
	}
	// WITHOUT it (per-user root), the file isn't there — the fix would no-op.
	if got := buildChangedFilesSection(act, j, "u1", ""); got != "" {
		t.Errorf("per-user root must NOT find the per-chat file, got:\n%s", got)
	}
}

// TestActivityWrittenTracksCwd verifies write/edit paths are captured
// jail-relative, resolved against the cwd a prior cd established.
func TestActivityWrittenTracksCwd(t *testing.T) {
	sess := newTestSession(t,
		fnCall("c1", "cd", map[string]any{"dir": "repo"}),
		fnResp("c1", "cd", map[string]any{"dir": "repo"}),
		fnCall("w1", "write_file", map[string]any{"path": "app/logic.ts"}),
		fnResp("w1", "write_file", map[string]any{"bytes": 42, "created": true}),
		fnCall("w2", "write_file", map[string]any{"path": "/toplevel.ts"}),
		fnResp("w2", "write_file", map[string]any{"bytes": 10, "created": true}),
	)
	act := activityFromSession(sess)
	want := []string{"repo/app/logic.ts", "toplevel.ts"}
	if len(act.written) != len(want) {
		t.Fatalf("written = %v, want %v", act.written, want)
	}
	for i, w := range want {
		if act.written[i] != w {
			t.Errorf("written[%d] = %q, want %q", i, act.written[i], w)
		}
	}
}

// TestGitCloneCountsAsRetrieval reenacts the live routing-failure's second
// half (2026-07-10): a node following the research-git-repos flow CLONES a
// repo instead of web-fetching it, then cites the repo URL — citationScore
// scored it 0.00 backing because git_clone landed in neither fetched nor
// seen. A successful clone must enter act.clonedRepos/clonedDirs (full-backing
// credit: the repo's whole contents are locally available), and the ledger
// entry is unchanged.
func TestGitCloneCountsAsRetrieval(t *testing.T) {
	svc := session.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	sess := resp.Session

	const repoURL = "https://github.com/example/repo"
	call := session.NewEvent(ctx, "test")
	call.Author = "coder"
	call.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{ID: "c1", Name: "git_clone",
			Args: map[string]any{"url": repoURL, "dir": "repo"}},
	}}}
	if err := svc.AppendEvent(ctx, sess, call); err != nil {
		t.Fatal(err)
	}
	respEv := session.NewEvent(ctx, "test")
	respEv.Author = "coder"
	respEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "git_clone",
			Response: map[string]any{"dir": "repo", "head": "abc123", "default_branch": "main"}},
	}}}
	if err := svc.AppendEvent(ctx, sess, respEv); err != nil {
		t.Fatal(err)
	}

	act := activityFromSession(sess)

	// The clone is recorded structurally: URL + local dir.
	if len(act.clonedRepos) != 1 || act.clonedRepos[0] != repoURL {
		t.Fatalf("act.clonedRepos = %v, want [%s]", act.clonedRepos, repoURL)
	}
	if len(act.clonedDirs) != 1 || act.clonedDirs[0] != "repo" {
		t.Fatalf("act.clonedDirs = %v, want [repo]", act.clonedDirs)
	}
	// citationScore gives a citation of the cloned repo full backing.
	answer := "The repo's entrypoint is documented in [the repository](" + repoURL + ")."
	score, details, ok := citationScore(answer, act)
	if !ok {
		t.Fatal("citationScore abstained despite recorded retrieval")
	}
	if score != 1.0 {
		t.Errorf("citationScore = %v, want 1.0 backing for the cloned repo URL (details: %+v)", score, details)
	}
	// Ledger behavior unchanged: the git_clone op is still recorded.
	if len(act.workspace) != 1 || act.workspace[0].tool != "git_clone" {
		t.Errorf("workspace ledger = %+v, want the git_clone op recorded", act.workspace)
	}
}

// TestGitCloneFailureGetsNoRetrievalCredit: a FAILED clone earns no grounding
// (nothing was retrieved) while still appearing in the ledger as a FAILED op
// the judge can hold against claims.
func TestGitCloneFailureGetsNoRetrievalCredit(t *testing.T) {
	svc := session.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	sess := resp.Session

	const repoURL = "https://github.com/example/missing"
	call := session.NewEvent(ctx, "test")
	call.Author = "coder"
	call.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{ID: "c1", Name: "git_clone", Args: map[string]any{"url": repoURL}},
	}}}
	if err := svc.AppendEvent(ctx, sess, call); err != nil {
		t.Fatal(err)
	}
	respEv := session.NewEvent(ctx, "test")
	respEv.Author = "coder"
	respEv.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "git_clone",
			Response: map[string]any{"error": "repository not found"}},
	}}}
	if err := svc.AppendEvent(ctx, sess, respEv); err != nil {
		t.Fatal(err)
	}

	act := activityFromSession(sess)
	if len(act.clonedRepos) != 0 || len(act.clonedDirs) != 0 {
		t.Errorf("a FAILED clone must not earn retrieval credit (clonedRepos=%v clonedDirs=%v)", act.clonedRepos, act.clonedDirs)
	}
	if len(act.workspace) != 1 || !strings.Contains(act.workspace[0].detail, "FAILED") {
		t.Errorf("workspace ledger = %+v, want the FAILED git_clone recorded", act.workspace)
	}
}
