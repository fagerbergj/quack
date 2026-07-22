package tools

import (
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
)

// testToolCtx is a functional agent.Context for driving a tool's Run handler
// directly in a test: adkagent.ContextMock covers everything the tool doesn't
// use (panicking if it's ever called), and the embedded real context.Context
// backs Deadline/Done/Err/Value - needed because the handler wraps ctx in
// context.WithTimeout.
type testToolCtx struct {
	*adkagent.ContextMock
	ctx context.Context
}

func newTestToolCtx(ctx context.Context) *testToolCtx {
	return &testToolCtx{ContextMock: &adkagent.ContextMock{}, ctx: ctx}
}

func (c *testToolCtx) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c *testToolCtx) Done() <-chan struct{}       { return c.ctx.Done() }
func (c *testToolCtx) Err() error                  { return c.ctx.Err() }
func (c *testToolCtx) Value(key any) any           { return c.ctx.Value(key) }

func TestGitHubPRContext_RoundTrips(t *testing.T) {
	if _, ok := GitHubPRFromContext(context.Background()); ok {
		t.Fatal("bare context should carry no GitHubPR")
	}
	want := GitHubPR{Owner: "acme", Repo: "games", Number: 246}
	ctx := WithGitHubPR(context.Background(), want.Owner, want.Repo, want.Number)
	got, ok := GitHubPRFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("GitHubPRFromContext = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestFalsePositiveCandidate_RequiresFindingAndReason(t *testing.T) {
	pr := GitHubPR{Owner: "acme", Repo: "games", Number: 246}
	cases := map[string]correctReviewFindingArgs{
		"missing finding": {Reason: "r"},
		"missing reason":  {Finding: "f"},
		"both blank":      {Finding: "  ", Reason: "  "},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := falsePositiveCandidate(pr, a); err == nil {
				t.Fatalf("falsePositiveCandidate(%+v) should error", a)
			}
		})
	}
}

func TestFalsePositiveCandidate_ScopesToTheWiredPR(t *testing.T) {
	pr := GitHubPR{Owner: "Acme", Repo: "Games", Number: 246}
	sc, cand, err := falsePositiveCandidate(pr, correctReviewFindingArgs{
		Finding: `empty Comment.Body breaks dispatch via triggerTask`,
		Reason:  "dispatch takes the task string directly, it never calls triggerTask",
	})
	if err != nil {
		t.Fatalf("falsePositiveCandidate: %v", err)
	}
	// Same key format as workspace.RepoIdentity ("github.com/owner/repo",
	// lowercased) - the exact bucket the gate's memoryScope resolves for a
	// coding node working in this repo.
	if sc.Repo != "github.com/acme/games" {
		t.Fatalf("scope repo = %q, want github.com/acme/games", sc.Repo)
	}
	if sc.Role != memory.RoleCoding {
		t.Fatalf("scope role = %q, want %q", sc.Role, memory.RoleCoding)
	}
	for _, want := range []string{"PR #246", "triggerTask", "dispatch takes the task string directly"} {
		if !strings.Contains(cand.Content, want) {
			t.Fatalf("candidate content missing %q: %q", want, cand.Content)
		}
	}
	if cand.Metadata["kind"] != "false_positive" {
		t.Fatalf("candidate kind = %q, want false_positive", cand.Metadata["kind"])
	}
}

func TestNewCorrectReviewFindingTool(t *testing.T) {
	tl, err := NewCorrectReviewFindingTool(nil, GitHubPR{}) // construction only; handler not invoked
	if err != nil {
		t.Fatalf("NewCorrectReviewFindingTool: %v", err)
	}
	if tl.Name() != "correct_review_finding" {
		t.Fatalf("name = %q, want correct_review_finding", tl.Name())
	}
}

// runnable is the subset of functiontool's generated Tool this test drives
// directly - the real handler, not just construction.
type runnable interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// fakeEmbedder returns a fixed unit vector for every text, so any recall query
// matches any stored point (cosine = 1) - enough to exercise Commit + Recall
// without a real embedding model.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// addOneConsolidator ADDs each "- <content>" staged-candidate line it's given,
// verbatim - the consolidation reply memory.Store.Commit expects. Mirrors
// internal/memory's echoModel test double, but marshals with encoding/json
// instead of string-concatenating into a JSON literal: the correction text
// contains quotes and colons that would otherwise break the literal.
type addOneConsolidator struct{}

func (addOneConsolidator) Name() string { return "fake-consolidator" }

type consolidateOp struct {
	Action  string `json:"action"`
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

func (addOneConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var prompt strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			prompt.WriteString(p.Text)
		}
	}
	var ops []consolidateOp
	for _, line := range strings.Split(prompt.String(), "\n") {
		if content, ok := strings.CutPrefix(line, "- "); ok {
			ops = append(ops, consolidateOp{Action: "ADD", Content: content, Kind: "false_positive"})
		}
	}
	reply, _ := json.Marshal(struct {
		Ops []consolidateOp `json:"ops"`
	}{Ops: ops})
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: string(reply)}}}}, nil)
	}
}

func newTestTaskStore(t *testing.T) *memory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := memory.New(context.Background(), "sqlite", path, fakeEmbedder{}, addOneConsolidator{}, "test_task", "task", 5, 0.5)
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return s
}

// TestCorrectReviewFindingTool_WritesOnlyTheWiredPR is the handler-level
// acceptance test for #249's hardening: a correction lands in the coding
// bucket for the repo/PR the conversation is ACTUALLY on (never one supplied
// by the model), and is recalled through the exact scope a code-reviewer node
// would use for that repo.
func TestCorrectReviewFindingTool_WritesOnlyTheWiredPR(t *testing.T) {
	ctx := context.Background()
	store := newTestTaskStore(t)
	pr := GitHubPR{Owner: "acme", Repo: "games", Number: 246}

	tl, err := NewCorrectReviewFindingTool(store, pr)
	if err != nil {
		t.Fatalf("NewCorrectReviewFindingTool: %v", err)
	}
	r, ok := tl.(runnable)
	if !ok {
		t.Fatalf("%T does not implement Run(agent.Context, any)", tl)
	}

	// The legitimate call: only finding/reason, exactly what the schema now
	// declares. It must land on the repo/PR wired into the tool.
	mockCtx := newTestToolCtx(ctx)
	if _, err := r.Run(mockCtx, map[string]any{
		"finding": "empty Comment.Body breaks dispatch via triggerTask",
		"reason":  "dispatch takes the task string directly, it never calls triggerTask",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The reviewer's own gate-side recall scope for the REAL repo must see it.
	got := store.Recall(ctx, memory.Scope{Repo: "github.com/acme/games", Role: memory.RoleCoding}, "does an empty Comment.Body break dispatch")
	if got == "" {
		t.Fatal("correction not recalled through the wired repo's coding bucket")
	}
	if !strings.Contains(got, "NOT a real issue") {
		t.Fatalf("recalled memory = %q, missing the correction", got)
	}
}

// TestCorrectReviewFindingTool_ForgedRepoArgsRefused is the cross-repo-write
// regression test: a hostile/confused call tries to redirect the write to a
// DIFFERENT repo/PR by stuffing owner/repo/pr_number into the args - fields
// the schema no longer declares (owner/repo/pr_number come ONLY from the
// verified GitHubPR the tool was constructed with, never the model). The
// generated JSON schema rejects unknown properties outright, so the call is
// refused wholesale rather than silently redirected or partially applied.
func TestCorrectReviewFindingTool_ForgedRepoArgsRefused(t *testing.T) {
	ctx := context.Background()
	store := newTestTaskStore(t)
	pr := GitHubPR{Owner: "acme", Repo: "games", Number: 246}

	tl, err := NewCorrectReviewFindingTool(store, pr)
	if err != nil {
		t.Fatalf("NewCorrectReviewFindingTool: %v", err)
	}
	r := tl.(runnable)
	mockCtx := newTestToolCtx(ctx)
	if _, err := r.Run(mockCtx, map[string]any{
		"finding":   "empty Comment.Body breaks dispatch via triggerTask",
		"reason":    "dispatch takes the task string directly, it never calls triggerTask",
		"owner":     "evil",
		"repo":      "other-repo",
		"pr_number": 999,
	}); err == nil {
		t.Fatal("Run with forged owner/repo/pr_number should be refused, got no error")
	}

	// Nothing was written anywhere - neither the real repo nor the forged one.
	if got := store.Recall(ctx, memory.Scope{Repo: "github.com/acme/games", Role: memory.RoleCoding}, "does an empty Comment.Body break dispatch"); got != "" {
		t.Fatalf("a refused call must write nothing, got %q", got)
	}
	if got := store.Recall(ctx, memory.Scope{Repo: "github.com/evil/other-repo", Role: memory.RoleCoding}, "does an empty Comment.Body break dispatch"); got != "" {
		t.Fatalf("forged owner/repo recalled a memory, want none: %q", got)
	}
}

// TestCorrectReviewFindingTool_MissingFieldsRejected proves the handler
// refuses to write when the model omits the required correction fields -
// the ONE thing it still trusts the model for, and even that is validated.
func TestCorrectReviewFindingTool_MissingFieldsRejected(t *testing.T) {
	ctx := context.Background()
	store := newTestTaskStore(t)
	pr := GitHubPR{Owner: "acme", Repo: "games", Number: 246}

	tl, err := NewCorrectReviewFindingTool(store, pr)
	if err != nil {
		t.Fatalf("NewCorrectReviewFindingTool: %v", err)
	}
	r := tl.(runnable)
	mockCtx := newTestToolCtx(ctx)
	if _, err := r.Run(mockCtx, map[string]any{"finding": "", "reason": ""}); err == nil {
		t.Fatal("Run with no finding/reason should error")
	}
	if got := store.Recall(ctx, memory.Scope{Repo: "github.com/acme/games", Role: memory.RoleCoding}, "anything"); got != "" {
		t.Fatalf("a rejected call must write nothing, got %q", got)
	}
}
