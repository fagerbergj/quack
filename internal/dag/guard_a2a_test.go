package dag_test

// Guard-ladder confirm tier over the A2A hop — the boundary that already bit
// ask_advisor twice (see TestAskAdvisor_OverA2A) and then bit the guard live:
// a guarded tool executes inside the A2A SERVER's runner, whose own context
// session (AppName = the agent name, fresh per gate round) holds NONE of the
// confirm pause/resume events. The guard used to scan THAT session, found
// nothing, and re-requested confirmation on every approved re-issue — the
// approval could never be consumed. These tests run the full production
// shape: gated node → A2A client → loopback A2A server → worker llmagent with
// a REAL confirm-guarded tool (delete_path via tools.Build) → pause → human
// decision → resume, over the durable DB-backed session service.

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// guardA2AStub drives the worker + judge for the confirm-over-A2A tests:
//   - judge requests (submit_verdict) always pass;
//   - once the request history carries the guarded tool's RESOLVED response
//     (post-approval real execution), write the final answer;
//   - a post-decision prompt saying APPROVED re-issues the delete with
//     approvedPath (same as the original for the consume case, different for
//     the pinning case);
//   - a post-decision prompt saying DENIED answers without the operation;
//   - otherwise (fresh draft) propose deleting victim.txt.
type guardA2AStub struct {
	mu           sync.Mutex
	approvedPath string
}

func (*guardA2AStub) Name() string { return "guardA2AStub" }

func (s *guardA2AStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		txt := atAllText(req)
		if reqHasResolvedGuardResponse(req, "delete_path") {
			yield(atText("FINAL: deletion performed."), nil)
			return
		}
		switch {
		case strings.Contains(txt, "DENIED"):
			yield(atText("FINAL: completed without deleting."), nil)
		case strings.Contains(txt, "APPROVED"):
			s.mu.Lock()
			p := s.approvedPath
			s.mu.Unlock()
			yield(atCall("delete_path", map[string]any{"path": p}), nil)
		default:
			yield(atCall("delete_path", map[string]any{"path": "victim.txt"}), nil)
		}
	}
}

// reqHasResolvedGuardResponse reports whether the request history carries a
// FunctionResponse for name marked with vetting.GuardResolvedKey — i.e. the
// guarded tool already executed for real (or refused post-denial) this round.
func reqHasResolvedGuardResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != name {
				continue
			}
			if v, ok := p.FunctionResponse.Response[vetting.GuardResolvedKey].(bool); ok && v {
				return true
			}
		}
	}
	return false
}

// guardA2ARun bundles the pieces each test drives: a REAL confirm-guarded
// delete_path (tools.Build with Guards, bound to a temp jail), a worker
// llmagent served over loopback A2A, and the durable sqlite-backed session
// service shared by the A2A server, the executor, and the guard (exactly how
// internal/serve wires st.Sessions everywhere).
type guardA2ARun struct {
	stub     *guardA2AStub
	sessions session.Service
	jail     *workspace.Jail
	plan     dag.Plan
	run      func(content *genai.Content, resume []string) (paused bool, outputs map[string]string, events []stream.SSEEvent)
}

func newGuardA2ARun(t *testing.T) *guardA2ARun {
	t.Helper()
	stub := &guardA2AStub{approvedPath: "victim.txt"}

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sessions := st.Sessions

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	// Seed the jailed user dir with the file(s) the stub proposes deleting.
	writeJailFile(t, jail, "u1", "victim.txt")
	writeJailFile(t, jail, "u1", "other.txt")

	builtins, err := tools.Build([]string{"delete_path"}, tools.Deps{
		Workspace:       jail,
		WorkspaceUserID: "u1",
		Sessions:        sessions,
		Guards:          map[string]string{"delete_path": "confirm"},
	})
	if err != nil {
		t.Fatalf("tools.Build: %v", err)
	}

	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Do the work.",
		Tools: []tool.Tool{builtins[0]},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.Client()
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "clean up the workspace", Rubric: "workspace tidy"},
	}}
	return &guardA2ARun{
		stub: stub, sessions: sessions, jail: jail, plan: plan,
		run: func(content *genai.Content, resume []string) (bool, map[string]string, []stream.SSEEvent) {
			return runGraph(t, client, stub, sessions, plan, content, resume)
		},
	}
}

func writeJailFile(t *testing.T, jail *workspace.Jail, userID, rel string) {
	t.Helper()
	real, err := jail.Resolve(userID, rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jailFileExists(t *testing.T, jail *workspace.Jail, userID, rel string) bool {
	t.Helper()
	real, err := jail.Resolve(userID, rel)
	if err != nil {
		t.Fatal(err)
	}
	_, serr := os.Stat(real)
	return serr == nil
}

// pauseFromEvents extracts the last node_needs_input (interrupt ID, message).
func pauseFromEvents(events []stream.SSEEvent) (id, msg string) {
	for _, ev := range events {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			id, msg = d.InterruptID, d.Message
		}
	}
	return id, msg
}

func confirmResume(interruptID, decision string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": decision},
		},
	}}}
}

// TestGuardConfirm_OverA2A_ApprovalConsumed reproduces the LIVE failure: over
// A2A, an approved same-args re-issue must EXECUTE (consume the pinned
// approval), not re-request confirmation forever. The old guard scanned the
// A2A context session (its own ctx coordinates), found no confirm events, and
// looped.
func TestGuardConfirm_OverA2A_ApprovalConsumed(t *testing.T) {
	h := newGuardA2ARun(t)

	// Run 1: the worker proposes delete_path(victim.txt) → confirm pause.
	paused, out1, ev1 := h.run(&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused || out1["n1"] != "" {
		t.Fatalf("run1: want paused with no output, got paused=%v out=%q", paused, out1["n1"])
	}
	pauseID, pauseMsg := pauseFromEvents(ev1)
	if pauseID != "confirm-n1-r1" {
		t.Fatalf("run1: interrupt = %q, want confirm-n1-r1", pauseID)
	}
	if !strings.Contains(pauseMsg, "delete_path") {
		t.Errorf("run1: pause message %q does not name the operation", pauseMsg)
	}
	if !jailFileExists(t, h.jail, "u1", "victim.txt") {
		t.Fatal("run1: victim.txt deleted before approval")
	}

	// Run 2: the human approves; the worker re-issues the SAME call. It must
	// execute exactly once — the run COMPLETES (no second confirmation) and
	// the file is actually gone.
	paused2, out2, ev2 := h.run(confirmResume(pauseID, "approve"), []string{"n1"})
	if paused2 {
		id2, msg2 := pauseFromEvents(ev2)
		t.Fatalf("run2: paused AGAIN (%q: %q) — the approval was never consumed (the live A2A bug)", id2, msg2)
	}
	if !strings.Contains(out2["n1"], "deletion performed") {
		t.Errorf("run2: out = %q, want the post-execution answer", out2["n1"])
	}
	if jailFileExists(t, h.jail, "u1", "victim.txt") {
		t.Error("run2: victim.txt still exists — the approved operation never executed")
	}
	if !jailFileExists(t, h.jail, "u1", "other.txt") {
		t.Error("run2: other.txt was deleted — executed with the wrong args")
	}
}

// TestGuardConfirm_OverA2A_DifferentArgsReProposes: args-pinning still holds
// over the A2A hop — an approved-then-swapped-args call must NOT execute and
// must raise a fresh confirmation that warns it DIFFERS.
func TestGuardConfirm_OverA2A_DifferentArgsReProposes(t *testing.T) {
	h := newGuardA2ARun(t)
	h.stub.mu.Lock()
	h.stub.approvedPath = "other.txt" // swap args after approval
	h.stub.mu.Unlock()

	paused, _, ev1 := h.run(&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused {
		t.Fatal("run1: expected a pause")
	}
	pauseID, _ := pauseFromEvents(ev1)

	paused2, out2, ev2 := h.run(confirmResume(pauseID, "approve"), []string{"n1"})
	if !paused2 {
		t.Fatalf("run2: want a second pause for the swapped args, got completion out=%q", out2["n1"])
	}
	id2, msg2 := pauseFromEvents(ev2)
	if id2 != "confirm-n1-r2" {
		t.Errorf("run2: interrupt = %q, want confirm-n1-r2", id2)
	}
	if !strings.Contains(msg2, "DIFFERS") {
		t.Errorf("run2: pause message %q does not warn the operation DIFFERS", msg2)
	}
	if !jailFileExists(t, h.jail, "u1", "other.txt") || !jailFileExists(t, h.jail, "u1", "victim.txt") {
		t.Error("run2: a file was deleted without approval for those args")
	}
}
