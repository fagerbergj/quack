package vetting

import (
	"context"
	"iter"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

// #762: an ACP worker commits directly to the clone on disk, outside quack's
// session ledger - so a rejected round's commit is invisible to everything
// except a probe that reads the clone itself. strayCommitStub reproduces the
// production sequence: round 0 commits something unrelated to the task and
// the judge zeroes it, the revise round commits the real task and the judge
// passes it - all in the SAME clone, exactly as RunGatedRefine drives it.
type strayCommitStub struct {
	t       *testing.T
	dir     string
	judgeN  int
	workerN int
}

func (m *strayCommitStub) Name() string { return "stray-commit-stub" }

func (m *strayCommitStub) git(args ...string) {
	m.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", m.dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		m.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (m *strayCommitStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeN++
			if m.judgeN == 1 {
				// The judge catching the off-task round in production: all three
				// named criteria at zero (#762's observed verdict).
				yield(stubCall(submitVerdictTool, map[string]any{
					"criteria": map[string]any{
						"task_completeness":     map[string]any{"score": 0.0, "reason": "implemented an unrelated feature"},
						"commit_hygiene":        map[string]any{"score": 0.0, "reason": "committed unrequested work"},
						"claims_match_activity": map[string]any{"score": 0.0, "reason": "unrelated to the task"},
					},
					"score": 0.0, "feedback": "you committed GCS storage support, which nobody asked for; implement the CD workflow instead",
				}), nil)
				return
			}
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"task_completeness": map[string]any{"score": 1.0, "reason": "the CD workflow is correct"}},
				"score":    0.95, "feedback": "",
			}), nil)
			return
		}
		m.workerN++
		if strings.Contains(stubAllText(req), "Verdict:") {
			// Revise round: do the actual task.
			writeFile(m.t, filepath.Join(m.dir, "publish.yml"), "name: publish\n")
			m.git("add", "-A")
			m.git("commit", "-q", "-m", "Add CD publishing to GHCR")
			yield(stubText("Added the CD workflow publishing to GHCR."), nil)
			return
		}
		// Draft round: commit something nobody asked for.
		writeFile(m.t, filepath.Join(m.dir, "gcs.go"), "package store\n")
		m.git("add", "-A")
		m.git("commit", "-q", "-m", "feat(store): add GCS-backed artifact storage")
		yield(stubText("Added GCS-backed artifact storage."), nil)
	}
}

// strayCommitTestRepo builds a setup-style clone (base commit, work branch
// checked out, nothing on it yet) - the state SetupClone leaves a fresh run in.
func strayCommitTestRepo(t *testing.T) (Config, string) {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := jail.EnsureDir("u1", "c1", workspace.SetupCloneDir("impl"))
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "base.txt"), "base")
	git("add", "-A")
	git("commit", "-q", "-m", "base commit")
	git("checkout", "-q", "-b", "quack/work")

	cfg := Config{
		JudgeRounds:     1,
		Threshold:       0.7,
		Rubric:          "score 0-10",
		NodeID:          "impl",
		Workspace:       jail,
		WorkspaceUserID: "u1",
		ChatID:          "c1",
		WorkspaceCaps:   workspace.DefaultCaps(),
		Setup:           &SetupBranch{Repo: "https://example.com/r.git", WorkBranch: "quack/work"},
		Deliver:         func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) { return nil, nil },
	}
	return cfg, dir
}

func runStrayCommitGate(t *testing.T, stub model.LLM, cfg Config) {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "implementer", Instruction: "Do the task.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	// cfg.NodeBaseSHA mirrors what RunGatedRefine stamps at entry (node.go:206) -
	// this test drives RunGatedRefine directly via newTestGatedNode, which does
	// stamp it, so nothing extra is needed here; kept for documentation.
	node, err := newTestGatedNode("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Add a CD workflow publishing to GHCR."}}}
	deadline := time.Now().Add(10 * time.Second)
	for _, rerr := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
		if time.Now().After(deadline) {
			t.Fatal("gate run did not finish in time")
		}
	}
}

// TestGate1_RejectedRoundsCommitNeverSurvivesToDelivery is issue #762 test case
// 1: a rejected round's commit must not still be on the branch once a later
// round passes and the gate delivers. This FAILS on main - nothing resets the
// clone between a failed judge round and the revise round it triggers, so the
// off-task commit from the draft round rides along into the branch the
// passing revise round delivers.
func TestGate1_RejectedRoundsCommitNeverSurvivesToDelivery(t *testing.T) {
	cfg, dir := strayCommitTestRepo(t)
	stub := &strayCommitStub{t: t, dir: dir}
	runStrayCommitGate(t, stub, cfg)

	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "log", "--format=%s", "main..quack/work"}, workspace.DefaultCaps())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git log: %v (exit %d): %s", err, res.ExitCode, res.Output)
	}
	if strings.Contains(res.Output, "GCS") {
		t.Fatalf("the rejected round's stray commit survived to the branch the gate delivers:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "Add CD publishing to GHCR") {
		t.Fatalf("the passing round's own commit is missing from the branch:\n%s", res.Output)
	}
}

// TestGate2_NormalRunDeliversOnlyItsOwnCommits is issue #762 test case 2: a
// clean single-round pass must be unaffected by the reset (nothing to undo).
func TestGate2_NormalRunDeliversOnlyItsOwnCommits(t *testing.T) {
	cfg, dir := strayCommitTestRepo(t)
	cfg.JudgeRounds = 1
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	stub := &cleanPassStub{t: t, dir: dir, git: git}
	runStrayCommitGate(t, stub, cfg)

	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "log", "--format=%s", "main..quack/work"}, workspace.DefaultCaps())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git log: %v (exit %d): %s", err, res.ExitCode, res.Output)
	}
	got := strings.TrimSpace(res.Output)
	want := "Add CD publishing to GHCR"
	if got != want {
		t.Fatalf("branch commits = %q, want only %q (a passing round must not be reset)", got, want)
	}
}

// incompleteOnTaskStub commits real, on-task work in the draft round that the
// judge rejects for being short of the task (missing .dockerignore, say) -
// commit_hygiene itself scores fine. The revise round adds a SECOND file/commit
// rather than redoing the first, the way an ACP worker naturally continues
// when its own prior commit is still sitting in the clone.
type incompleteOnTaskStub struct {
	t      *testing.T
	dir    string
	git    func(args ...string)
	judgeN int
}

func (m *incompleteOnTaskStub) Name() string { return "incomplete-on-task-stub" }

func (m *incompleteOnTaskStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeN++
			if m.judgeN == 1 {
				yield(stubCall(submitVerdictTool, map[string]any{
					"criteria": map[string]any{
						"commit_hygiene":    map[string]any{"score": 0.9, "reason": "well-scoped commit, on task"},
						"task_completeness": map[string]any{"score": 0.3, "reason": "missing .dockerignore"},
					},
					"score": 0.3, "feedback": "add the .dockerignore too",
				}), nil)
				return
			}
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"task_completeness": map[string]any{"score": 1.0, "reason": "complete now"}},
				"score":    0.95, "feedback": "",
			}), nil)
			return
		}
		if strings.Contains(stubAllText(req), "Verdict:") {
			writeFile(m.t, filepath.Join(m.dir, ".dockerignore"), "node_modules\n")
			m.git("add", "-A")
			m.git("commit", "-q", "-m", "Add .dockerignore")
			yield(stubText("Added the missing .dockerignore."), nil)
			return
		}
		writeFile(m.t, filepath.Join(m.dir, "publish.yml"), "name: publish\n")
		m.git("add", "-A")
		m.git("commit", "-q", "-m", "Add CD publishing to GHCR")
		yield(stubText("Added the CD workflow publishing to GHCR."), nil)
	}
}

// TestGate4_IncompleteButOnTaskRoundIsNotReset: the fourth case the
// coordinator asked for on top of the issue's three - a round rejected for
// being INCOMPLETE, not off-task, must keep its commit so the revise round
// builds on it. This fails before the commit_hygiene keying: an unconditional
// reset on every judge failure wipes the draft's "Add CD publishing to GHCR"
// commit right along with the (nonexistent, here) contamination.
func TestGate4_IncompleteButOnTaskRoundIsNotReset(t *testing.T) {
	cfg, dir := strayCommitTestRepo(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	stub := &incompleteOnTaskStub{t: t, dir: dir, git: git}
	runStrayCommitGate(t, stub, cfg)

	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "log", "--format=%s", "main..quack/work"}, workspace.DefaultCaps())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git log: %v (exit %d): %s", err, res.ExitCode, res.Output)
	}
	got := res.Output
	if !strings.Contains(got, "Add CD publishing to GHCR") {
		t.Fatalf("the rejected-but-on-task draft commit was wiped, not built on:\n%s", got)
	}
	if !strings.Contains(got, "Add .dockerignore") {
		t.Fatalf("the revise round's own commit is missing:\n%s", got)
	}
}

// cleanPassStub commits the real task once and passes the judge immediately -
// the ordinary, uncontaminated single-round case.
type cleanPassStub struct {
	t       *testing.T
	dir     string
	git     func(args ...string)
	drafted bool
}

func (m *cleanPassStub) Name() string { return "clean-pass-stub" }

func (m *cleanPassStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"task_completeness": map[string]any{"score": 1.0, "reason": "correct"}},
				"score":    0.95, "feedback": "",
			}), nil)
			return
		}
		if !m.drafted {
			m.drafted = true
			writeFile(m.t, filepath.Join(m.dir, "publish.yml"), "name: publish\n")
			m.git("add", "-A")
			m.git("commit", "-q", "-m", "Add CD publishing to GHCR")
		}
		yield(stubText("Added the CD workflow publishing to GHCR."), nil)
	}
}
