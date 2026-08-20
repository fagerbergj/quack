package serve

import (
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// resumeStubLLM answers judge calls with a passing verdict and records every
// worker prompt, so the test can see exactly which nodes did real work.
type resumeStubLLM struct {
	mu      sync.Mutex
	prompts []string
}

func (*resumeStubLLM) Name() string { return "resumeStub" }

func (s *resumeStubLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil {
			for _, tl := range req.Config.Tools {
				if tl == nil {
					continue
				}
				for _, fd := range tl.FunctionDeclarations {
					if fd != nil && fd.Name == "submit_verdict" {
						yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{Name: "submit_verdict", Args: map[string]any{"score": 0.9, "feedback": ""}},
						}}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
						return
					}
				}
			}
		}
		var b strings.Builder
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					b.WriteString(p.Text)
				}
			}
		}
		s.mu.Lock()
		s.prompts = append(s.prompts, b.String())
		s.mu.Unlock()
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "resumed output"}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (s *resumeStubLLM) workerPrompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// TestDriveResume_ReentryRunsPausedNodeOnly is the boot half of #962 end to
// end against real executor wiring: n1 done, n2 paused/shutdown (as the drain
// leaves it), n3 queued. driveResume must run n2's worker (not re-park on the
// persisted pause), schedule n3 as its descendant, leave n1 alone, and land
// n2 done on disk.
func TestDriveResume_ReentryRunsPausedNodeOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	chatID := "chat-boot-resume"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	userID := st.SessionUserForChat(ctx, chatID)

	plan := dag.Plan{ID: "plan-1", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "TASK-ONE"},
		{ID: "n2", AgentName: "blk", Task: "TASK-TWO", DependsOn: []string{"n1"}},
		{ID: "n3", AgentName: "blk", Task: "TASK-THREE", DependsOn: []string{"n2"}},
	}}
	planJSON, _ := json.Marshal(plan)
	if err := st.SaveDagPlan(ctx, chatID, plan.ID, "turn-1", string(planJSON)); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	// The state the last process left behind: n1 finished, n2 paused by the
	// drain, n3 never started.
	for _, n := range []store.DagNode{
		{PlanID: plan.ID, NodeID: "n1", Status: string(dag.StatusDone), Output: "ONE-OUT"},
		{PlanID: plan.ID, NodeID: "n2", Status: string(dag.StatusPaused), PauseReason: string(dag.PauseShutdown)},
		{PlanID: plan.ID, NodeID: "n3", Status: string(dag.StatusQueued)},
	} {
		if err := st.UpsertDagNode(ctx, n); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", n.NodeID, err)
		}
	}
	if err := st.SetNodeStatusForChat(ctx, chatID, "n2", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("persist pause: %v", err)
	}

	// Real executor + orchestrator over a stub LLM; the plan stashed in the
	// persisted session exactly as the execute tool leaves it.
	stub := &resumeStubLLM{}
	sessions := session.InMemoryService()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)
	orch := orchestrator.New(sessions, nil, "", nil, ex, nil, nil, nil)
	resp, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	_ = resp

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n2", Reason: dag.PauseShutdown}},
		orch, st, stream.NewHub())

	prompts := stub.workerPrompts()
	ran := func(task string) bool {
		for _, p := range prompts {
			if strings.Contains(p, task) {
				return true
			}
		}
		return false
	}
	if !ran("TASK-TWO") {
		t.Fatal("resumed node n2 never reached its worker - it re-parked on the persisted pause")
	}
	if !ran("TASK-THREE") {
		t.Error("descendant n3 was not scheduled by the re-entry")
	}
	if ran("TASK-ONE") {
		t.Error("done sibling n1 re-ran; re-entry must be the scoped node+descendants subset")
	}
	// PersistNodeEvent is synchronous, so the row is settled when driveResume returns.
	n2, err := st.GetDagNode(ctx, plan.ID, "n2")
	if err != nil || n2 == nil {
		t.Fatalf("GetDagNode n2: %v %v", n2, err)
	}
	if n2.Status != string(dag.StatusDone) || n2.PauseReason != "" {
		t.Errorf("n2 persisted as %q/%q, want done with the pause cleared", n2.Status, n2.PauseReason)
	}
	if n1, _ := st.GetDagNode(ctx, plan.ID, "n1"); n1 == nil || n1.Output != "ONE-OUT" {
		t.Errorf("n1 output changed; the seeded output should have been reused")
	}
}
