package dag

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// newInvalidateFixture builds an Executor over a jail holding a REAL git repo
// at the shared-clone scope, so the clean-tree gate runs against real git.
func newInvalidateFixture(t *testing.T) (*Executor, vetting.Config, string, *int32Counter) {
	t.Helper()
	requireChainGit(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	clone, err := jail.EnsureDir("u1", "c1", workspace.SetupCloneDir(workspace.SharedRepoScope))
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	runChainGit(t, clone, "init", "--quiet", "--initial-branch=main")
	runChainGit(t, clone, "config", "user.email", "q@example.com")
	runChainGit(t, clone, "config", "user.name", "quack")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runChainGit(t, clone, "add", "-A")
	runChainGit(t, clone, "commit", "--quiet", "-m", "seed")

	calls := &int32Counter{}
	e := &Executor{}
	e.SetSetup(func(_ context.Context, _, _, _ string, _ Setup) error {
		calls.inc()
		return nil
	})
	cfg := vetting.Config{Workspace: jail, WorkspaceUserID: "u1", ChatID: "c1", WorkspaceCaps: workspace.DefaultCaps()}
	return e, cfg, clone, calls
}

func reviewPlan() *Plan {
	return &Plan{
		Nodes: []Node{{ID: "review", AgentName: reviewerAgent}},
		Setup: &Setup{Repo: "https://example.com/r.git", BaseRef: "main", WorkBranch: "pr-head", Provisioned: true},
	}
}

// TestRefreshStaleSetupRecloneGates pins every gate on the destructive path:
// setupCloneAndBranch RemoveAll's the tree, so a refresh must happen only for
// a read-only node in a review-only plan with nothing uncommitted.
func TestRefreshStaleSetupRecloneGates(t *testing.T) {
	tests := []struct {
		name  string
		plan  func() *Plan
		node  Node
		stale bool
		dirty bool
		want  bool
	}{
		{"clean review-only reviewer", reviewPlan, Node{ID: "review", AgentName: reviewerAgent}, true, false, true},
		{"never invalidated", reviewPlan, Node{ID: "review", AgentName: reviewerAgent}, false, false, false},
		{"uncommitted work in the clone", reviewPlan, Node{ID: "review", AgentName: reviewerAgent}, true, true, false},
		{"writer node", func() *Plan {
			p := reviewPlan()
			p.Nodes = []Node{{ID: "impl", AgentName: implementerAgent}}
			return p
		}, Node{ID: "impl", AgentName: implementerAgent}, true, false, false},
		{"non-repo node in a review-only plan", func() *Plan {
			p := reviewPlan()
			p.Nodes = append(p.Nodes, Node{ID: "synth", AgentName: "synthesizer"})
			return p
		}, Node{ID: "synth", AgentName: "synthesizer"}, true, false, false},
		{"reviewer in a plan that also implements", func() *Plan {
			p := reviewPlan()
			p.Nodes = append(p.Nodes, Node{ID: "impl", AgentName: implementerAgent})
			return p
		}, Node{ID: "review", AgentName: reviewerAgent}, true, false, false},
		{"no setup", func() *Plan {
			p := reviewPlan()
			p.Setup = nil
			return p
		}, Node{ID: "review", AgentName: reviewerAgent}, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, cfg, clone, calls := newInvalidateFixture(t)
			if tt.dirty {
				if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("edited\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			staleSetups.Delete("c1")
			if tt.stale {
				MarkSetupStale("c1")
			}
			t.Cleanup(func() { staleSetups.Delete("c1") })

			plan := tt.plan()
			got := e.refreshStaleSetup(context.Background(), "u1", "c1", plan, tt.node, cfg)
			if got != tt.want {
				t.Fatalf("refreshStaleSetup = %v, want %v", got, tt.want)
			}
			wantCalls := 0
			if tt.want {
				wantCalls = 1
			}
			if calls.get() != wantCalls {
				t.Fatalf("setup executor calls = %d, want %d", calls.get(), wantCalls)
			}
			if _, stale := staleSetups.Load("c1"); tt.want && stale {
				t.Error("flag still set after a successful refresh")
			} else if !tt.want && tt.stale && !stale {
				t.Error("flag cleared without a refresh; a later boundary can no longer retry")
			}
		})
	}
}

// TestRefreshStaleSetupIsOncePerSignal: the flag is a one-shot, so a second
// read-only node in the same run doesn't re-clone behind the first.
func TestRefreshStaleSetupIsOncePerSignal(t *testing.T) {
	e, cfg, _, calls := newInvalidateFixture(t)
	staleSetups.Delete("c1")
	MarkSetupStale("c1")
	t.Cleanup(func() { staleSetups.Delete("c1") })

	plan := reviewPlan()
	node := Node{ID: "review", AgentName: reviewerAgent}
	if !e.refreshStaleSetup(context.Background(), "u1", "c1", plan, node, cfg) {
		t.Fatal("first refresh = false, want true")
	}
	if e.refreshStaleSetup(context.Background(), "u1", "c1", plan, node, cfg) {
		t.Fatal("second refresh = true, want false (no new push signal)")
	}
	if calls.get() != 1 {
		t.Fatalf("setup executor calls = %d, want 1", calls.get())
	}
}

// promptSnoopStub records the worker's own prompt (not the judge's round).
type promptSnoopStub struct {
	mu     sync.Mutex
	prompt string
}

func (*promptSnoopStub) Name() string { return "promptSnoopStub" }

func (s *promptSnoopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		if s.prompt == "" { // first worker round; a refine round re-wraps the task
			s.prompt = gUserText(req)
		}
		s.mu.Unlock()
		yield(gText("ANSWER with a source [1](http://x)"), nil)
	}
}

// TestRefreshedNodeTellsItsWorker: a node that started against a re-cloned
// tree must say so - an earlier node's output in the same prompt describes
// the pre-push state, and nothing else in the run flags the discontinuity.
func TestRefreshedNodeTellsItsWorker(t *testing.T) {
	for _, refreshed := range []bool{true, false} {
		name := "refreshed"
		if !refreshed {
			name = "not refreshed"
		}
		t.Run(name, func(t *testing.T) {
			plan := Plan{ID: "t-refresh", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: reviewerAgent}}}
			cfg := nodeGateConfig(plan, plan.Nodes[0], nil, func(string) vetting.Config { return writableGateCfg() }, "chat1", "")
			stub := &promptSnoopStub{}
			runSingleNode(t, plan, cfg, stub, func(context.Context, Node, vetting.Config) bool { return refreshed })

			stub.mu.Lock()
			defer stub.mu.Unlock()
			if got := strings.Contains(stub.prompt, "re-cloned at its current head"); got != refreshed {
				t.Fatalf("prompt mentions the mid-run refresh = %v, want %v\nprompt:\n%s", got, refreshed, stub.prompt)
			}
		})
	}
}

// TestStaleFlagClearedOnFreshRunKeptOnResume: a fresh run clones, so any
// earlier signal is spent; a resume never clones, so the branch really is
// still ahead of the tree and the flag has to survive for the first safe
// node boundary to act on.
func TestStaleFlagClearedOnFreshRunKeptOnResume(t *testing.T) {
	stub := &setupStub{}
	ag, err := llmagent.New(llmagent.Config{Name: implementerAgent, Model: stub, Description: "impl", Instruction: "ROLE Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{} }, nil)
	ex.SetSetup(func(context.Context, string, string, string, Setup) error { return errors.New("clone denied") })
	plan := Plan{
		ID: "p", UserMessage: "go",
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent, Task: "do it"}},
	}
	yield := func(stream.SSEEvent, error) bool { return true }
	t.Cleanup(func() { staleSetups.Delete("chat") })

	MarkSetupStale("chat")
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", nil, yield, map[string]string{}, nil); err == nil {
		t.Fatal("expected the failing setup to abort the fresh run")
	}
	if _, stale := staleSetups.Load("chat"); stale {
		t.Error("flag survived a fresh run, which had just cloned")
	}

	MarkSetupStale("chat")
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", nil, yield, map[string]string{}, []string{"impl"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, stale := staleSetups.Load("chat"); !stale {
		t.Error("flag dropped on resume, where setup never runs to justify it")
	}
}
