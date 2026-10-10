package dag_test

// Guard confirm tier over the A2A hop: the guarded tool runs in the A2A server's own session,
// so the gate must find confirm events there or an approval can never be consumed.

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
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
	adkagent "google.golang.org/adk/v2/agent"
)

// guardA2AStub: judge always passes; a resolved guarded response finishes; APPROVED
// re-issues the delete at approvedPath; DENIED answers without it; otherwise propose it.
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

// reqHasResolvedGuardResponse reports whether name's FunctionResponse carries
// vetting.GuardResolvedKey, i.e. the guarded tool already ran (or refused) this round.
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

// guardA2ARun bundles a real confirm-guarded tool on a temp jail, a worker served over
// loopback A2A, and the sqlite session service shared everywhere, as serve wires it.
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

	// The guard ladder guards ExtTools by name, so a test-local wipe tool exercises the same
	// confirm-over-A2A path.
	wipe, err := functiontool.New[wipeArgs, wipeResult](
		functiontool.Config{Name: "delete_path", Description: "remove one jailed file"},
		func(ctx adkagent.Context, a wipeArgs) (wipeResult, error) {
			real, rerr := jail.Resolve("u1", runGraphChatID, filepath.Join(runGraphNodeID, a.Path))
			if rerr != nil {
				return wipeResult{}, rerr
			}
			if rerr := os.Remove(real); rerr != nil {
				return wipeResult{}, rerr
			}
			return wipeResult{Deleted: true}, nil
		})
	if err != nil {
		t.Fatalf("wipe tool: %v", err)
	}
	builtins, err := tools.Build([]string{"delete_path"}, tools.Deps{
		Workspace:       jail,
		WorkspaceUserID: "u1",
		Sessions:        sessions,
		Guards:          map[string]string{"delete_path": "confirm"},
		ExtTools:        map[string]tool.Tool{"delete_path": wipe},
		CallScope:       tools.CallScope{AdvisorToken: vetting.AdvisorThreadToken("p", "n1")}, // the node the plan below runs
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
	srv, err := quackagent.Serve(worker, sessions, nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", "test-ctx")
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

// writeJailFile seeds under the per-chat scope the run's tools resolve, not the per-user root.
func writeJailFile(t *testing.T, jail *workspace.Jail, userID, rel string) {
	t.Helper()
	real, err := jail.Resolve(userID, runGraphChatID, filepath.Join(runGraphNodeID, rel))
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

// jailFileExists checks the same per-chat scope writeJailFile seeded.
func jailFileExists(t *testing.T, jail *workspace.Jail, userID, rel string) bool {
	t.Helper()
	real, err := jail.Resolve(userID, runGraphChatID, filepath.Join(runGraphNodeID, rel))
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

// Over A2A an approved same-args re-issue must execute, not re-request confirmation forever.
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

	// Run 2: approve; the same-args re-issue executes once, the run completes, the file is gone.
	paused2, out2, ev2 := h.run(confirmResume(pauseID, "approve"), []string{"n1"})
	if paused2 {
		id2, msg2 := pauseFromEvents(ev2)
		t.Fatalf("run2: paused AGAIN (%q: %q) - the approval was never consumed (the live A2A bug)", id2, msg2)
	}
	if !strings.Contains(out2["n1"], "deletion performed") {
		t.Errorf("run2: out = %q, want the post-execution answer", out2["n1"])
	}
	if jailFileExists(t, h.jail, "u1", "victim.txt") {
		t.Error("run2: victim.txt still exists - the approved operation never executed")
	}
	if !jailFileExists(t, h.jail, "u1", "other.txt") {
		t.Error("run2: other.txt was deleted - executed with the wrong args")
	}
}

// guardReviseA2AStub proposes the guarded op only in a judge-fail revision: plain draft,
// judge fails, revision proposes delete (confirm pause), approval, re-issue.
type guardReviseA2AStub struct {
	mu     sync.Mutex
	calls  int
	judged int
}

func (*guardReviseA2AStub) Name() string { return "guardReviseA2AStub" }

func (s *guardReviseA2AStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judged++
			n := s.judged
			s.mu.Unlock()
			if n == 1 { // fail the first judge round so the gate revises
				yield(atCall("submit_verdict", map[string]any{"score": 0.3, "feedback": "not delivered yet"}), nil)
			} else {
				yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			}
			return
		}
		// Post-approval re-issue has executed for real → write the final answer.
		if reqHasResolvedGuardResponse(req, "delete_path") {
			yield(atText("FINAL: deletion performed."), nil)
			return
		}
		txt := atAllText(req)
		switch {
		case strings.Contains(txt, "APPROVED"): // resumed with the approval
			yield(atCall("delete_path", map[string]any{"path": "victim.txt"}), nil)
			return
		case strings.Contains(txt, "DENIED"):
			yield(atText("FINAL: completed without deleting."), nil)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 { // the DRAFT: coded, but proposes no guarded operation
			yield(atText("draft: implemented the feature; not yet delivered"), nil)
			return
		}
		// The REVISION (and any later plain worker turn): propose the guarded op.
		yield(atCall("delete_path", map[string]any{"path": "victim.txt"}), nil)
	}
}

// A confirmation first proposed in a revise round must still pause the node; approving
// it executes the operation exactly once.
func TestGuardConfirm_OverA2A_RaisedDuringRevision(t *testing.T) {
	stub := &guardReviseA2AStub{}

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sessions := st.Sessions

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	writeJailFile(t, jail, "u1", "victim.txt")

	// The guard ladder guards ExtTools by name, so a test-local wipe tool exercises the same
	// confirm-over-A2A path.
	wipe, err := functiontool.New[wipeArgs, wipeResult](
		functiontool.Config{Name: "delete_path", Description: "remove one jailed file"},
		func(ctx adkagent.Context, a wipeArgs) (wipeResult, error) {
			real, rerr := jail.Resolve("u1", runGraphChatID, filepath.Join(runGraphNodeID, a.Path))
			if rerr != nil {
				return wipeResult{}, rerr
			}
			if rerr := os.Remove(real); rerr != nil {
				return wipeResult{}, rerr
			}
			return wipeResult{Deleted: true}, nil
		})
	if err != nil {
		t.Fatalf("wipe tool: %v", err)
	}
	builtins, err := tools.Build([]string{"delete_path"}, tools.Deps{
		Workspace:       jail,
		WorkspaceUserID: "u1",
		Sessions:        sessions,
		Guards:          map[string]string{"delete_path": "confirm"},
		ExtTools:        map[string]tool.Tool{"delete_path": wipe},
		CallScope:       tools.CallScope{AdvisorToken: vetting.AdvisorThreadToken("p", "n1")}, // the node the plan below runs
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
	srv, err := quackagent.Serve(worker, sessions, nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "clean up the workspace", Rubric: "workspace tidy"},
	}}
	run := func(content *genai.Content, resume []string) (bool, map[string]string, []stream.SSEEvent) {
		return runGraph(t, client, stub, sessions, plan, content, resume)
	}

	// Run 1: draft (plain) → judge fail → revision proposes delete → MUST pause.
	paused, out1, ev1 := run(&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused {
		t.Fatalf("run1: the confirmation was raised in the REVISION and never paused the node (the live bug); out=%q", out1["n1"])
	}
	pauseID, pauseMsg := pauseFromEvents(ev1)
	if pauseID != "confirm-n1-r1" {
		t.Fatalf("run1: interrupt = %q, want confirm-n1-r1", pauseID)
	}
	if !strings.Contains(pauseMsg, "delete_path") {
		t.Errorf("run1: pause message %q does not name the operation", pauseMsg)
	}
	if !jailFileExists(t, jail, "u1", "victim.txt") {
		t.Fatal("run1: victim.txt deleted before approval")
	}

	// Run 2: approve → the operation executes exactly once and the run completes.
	paused2, out2, ev2 := run(confirmResume(pauseID, "approve"), []string{"n1"})
	if paused2 {
		id2, msg2 := pauseFromEvents(ev2)
		t.Fatalf("run2: paused AGAIN (%q: %q) - approval not consumed", id2, msg2)
	}
	if !strings.Contains(out2["n1"], "deletion performed") {
		t.Errorf("run2: out = %q, want the post-execution answer", out2["n1"])
	}
	if jailFileExists(t, jail, "u1", "victim.txt") {
		t.Error("run2: victim.txt still exists - the approved operation never executed")
	}
}

// Args-pinning holds over A2A: an approved-then-swapped-args call must not execute and
// must raise a fresh confirmation warning that it differs.
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

type wipeArgs struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type wipeResult struct {
	Deleted bool `json:"deleted"`
}
