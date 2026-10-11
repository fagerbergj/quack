package serve

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
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
			var hasReadFile, hasSubmitVerdict bool
			for _, tl := range req.Config.Tools {
				if tl == nil {
					continue
				}
				for _, fd := range tl.FunctionDeclarations {
					if fd == nil {
						continue
					}
					switch fd.Name {
					case "submit_verdict":
						hasSubmitVerdict = true
					case "read_file":
						hasReadFile = true
					}
				}
			}
			if hasSubmitVerdict {
				yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: "submit_verdict", Args: map[string]any{"score": 0.9, "feedback": ""}},
				}}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
				return
			}
			// A worker with a read_file tool: call it once (proving the workspace
			// scope resolved), then answer with whatever it read back.
			if hasReadFile && !hasFunctionResponse(req.Contents) {
				yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "README.md"}},
				}}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
				return
			}
			if hasReadFile {
				yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "read: " + functionResponseText(req.Contents)}}},
					FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
				return
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

// hasFunctionResponse reports whether req.Contents already carries a tool
// result - i.e. this is the round after the stub's own function call.
func hasFunctionResponse(contents []*genai.Content) bool {
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}

// functionResponseText extracts the read_file tool's "content" result field.
func functionResponseText(contents []*genai.Content) string {
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil {
				continue
			}
			if v, ok := p.FunctionResponse.Response["content"]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func (s *resumeStubLLM) workerPrompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// TestResumeGuardArchivedOrStale: archived chats never resume; plans past staleResumePlanCeiling fail,
// except awaiting_input (holds no slot). A user-paused node is never auto-resumed, regardless of age.
func TestResumeGuardArchivedOrStale(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name          string
		archived      bool
		hasPlan       bool
		pauseReason   dag.PauseReason
		planAge       time.Duration
		wantOK        bool
		wantReasonHas string
	}{
		{"fresh non-archived shutdown pause resumes", false, true, dag.PauseShutdown, time.Hour, true, ""},
		{"archived chat is never resumed even with a fresh plan", true, true, dag.PauseShutdown, time.Minute, false, "archived"},
		{"stale plan is failed even when not archived", false, true, dag.PauseShutdown, staleResumePlanCeiling + time.Minute, false, "stale"},
		{"no plan row skips the age check", false, false, dag.PauseShutdown, 0, true, ""},
		{"awaiting_input node past the ceiling is left as-is", false, true, dag.PauseAwaitingInput, staleResumePlanCeiling + time.Minute, true, ""},
		{"archived still wins over awaiting_input", true, true, dag.PauseAwaitingInput, staleResumePlanCeiling + time.Minute, false, "archived"},
		{"a human pause is never auto-resumed, even fresh", false, true, dag.PauseUser, time.Minute, false, "user"},
		{"archived still wins over a human pause", true, true, dag.PauseUser, time.Minute, false, "archived"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := resumeGuardArchivedOrStale(tc.archived, tc.hasPlan, tc.pauseReason, now.Add(-tc.planAge))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (why=%q)", ok, tc.wantOK, why)
			}
			if !tc.wantOK && !strings.Contains(why, tc.wantReasonHas) {
				t.Errorf("reason = %q, want it to mention %q", why, tc.wantReasonHas)
			}
		})
	}
}

// TestBoundedGoRun_CapsConcurrency pins the concurrency ceiling: at most
// `limit` of the 5 ids may be running at once.
func TestBoundedGoRun_CapsConcurrency(t *testing.T) {
	const limit = 2
	ids := []string{"c1", "c2", "c3", "c4", "c5"}
	release := make(chan struct{})
	defer close(release)

	var running, maxRunning atomic.Int32

	go boundedGoRun(ids, limit, func(string) {
		n := running.Add(1)
		for {
			old := maxRunning.Load()
			if n <= old || maxRunning.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		running.Add(-1)
	})

	// Give every goroutine a chance to acquire and block on release.
	time.Sleep(200 * time.Millisecond)
	if got := maxRunning.Load(); got > limit {
		t.Fatalf("max concurrently running = %d, want <= %d", got, limit)
	}
	if got := running.Load(); got != limit {
		t.Fatalf("currently running = %d, want exactly %d (the rest should be parked on the semaphore)", got, limit)
	}
}

// TestBoundedGoRun_DispatchDoesNotBlockOnFullSemaphore pins that dispatch
// returns immediately even when every id can't yet acquire a slot.
func TestBoundedGoRun_DispatchDoesNotBlockOnFullSemaphore(t *testing.T) {
	const maxConcurrent = 6 // fewer than len(ids), so the 7th id parks on the semaphore
	ids := []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"}

	release := make(chan struct{})
	defer close(release)
	returned := make(chan struct{})

	go func() {
		boundedGoRun(ids, maxConcurrent, func(string) {
			<-release // a real driveResume: minutes to hours
		})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("boundedGoRun blocked on the 7th id instead of parking a goroutine for it")
	}
}

// TestDriveResume_ReentryRunsPausedNodeOnly: n1 done, n2 paused/shutdown, n3 queued - driveResume runs n2
// (not re-parking on the persisted pause), schedules n3 as its descendant, and leaves n1 alone.
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
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)
	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	resp, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	_ = resp

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n2", Reason: dag.PauseShutdown}},
		orch, st, stream.NewHub(), runlog.NewEventLog(st), nil)

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

// newOneNodeResumeFixture: chatID's plan is one node n1 left paused by a shutdown, over a stub LLM.
func newOneNodeResumeFixture(t *testing.T, chatID string) (*store.Store, *orchestrator.Orchestrator, dag.Plan) {
	t.Helper()
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	userID := st.SessionUserForChat(ctx, chatID)

	plan := dag.Plan{ID: "plan-1node", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "TASK-ONE"},
	}}
	planJSON, _ := json.Marshal(plan)
	if err := st.SaveDagPlan(ctx, chatID, plan.ID, "turn-1", string(planJSON)); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, store.DagNode{PlanID: plan.ID, NodeID: "n1", Status: string(dag.StatusPaused), PauseReason: string(dag.PauseShutdown)}); err != nil {
		t.Fatalf("UpsertDagNode n1: %v", err)
	}
	if err := st.SetNodeStatusForChat(ctx, chatID, "n1", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("persist pause: %v", err)
	}

	stub := &resumeStubLLM{}
	sessions := session.InMemoryService()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)
	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}

	return st, orch, plan
}

// TestDriveResume_TailSurvivesCancelledRunCtx: the tail (StampTurn/StampTerminalOutcome/PendingQuestion)
// must not use runCtx - a cancel there would leave the chat showing as running forever.
func TestDriveResume_TailSurvivesCancelledRunCtx(t *testing.T) {
	ctx := context.Background()
	chatID := "chat-boot-resume-cancel"
	st, orch, plan := newOneNodeResumeFixture(t, chatID)

	hub := stream.NewHub()
	go func() {
		for !hub.HasRegisteredRun(chatID) {
			time.Sleep(time.Millisecond)
		}
		hub.CancelRun(chatID) // DrainActiveRuns' force-cancel, or a user cancel, mid-resume
	}()

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, hub, runlog.NewEventLog(st), nil)

	c, err := st.GetChat(ctx, chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v %v", c, err)
	}
	if c.ActiveTurnID != "" {
		t.Errorf("ActiveTurnID = %q, want cleared - the tail's stamp must survive runCtx being cancelled", c.ActiveTurnID)
	}
}

// TestDriveResume_ExtensionChatGetsRunEnded: the dispatching extension can only finalize (post, clear its
// pending row) a run resumed at boot if driveResume hands it the outcome.
func TestDriveResume_ExtensionChatGetsRunEnded(t *testing.T) {
	ctx := context.Background()
	chatID := "ext:obs:pr-1"
	st, orch, plan := newOneNodeResumeFixture(t, chatID)
	obs := &outcomeObserver{}
	exts := []builtSDKExtension{{name: "other", ext: &fakeRunObserver{}}, {name: "obs", ext: obs}}

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, stream.NewHub(), runlog.NewEventLog(st), runObserverFor(chatID, exts))

	if len(obs.outcomes) != 1 || obs.outcomes[0].Status != extsdk.RunDone || !obs.outcomes[0].PlanRan {
		t.Fatalf("RunEnded outcomes = %+v, want one RunDone with PlanRan", obs.outcomes)
	}
	if runObserverFor("chat-plain", exts) != nil || runObserverFor("ext:missing:x", exts) != nil {
		t.Error("only an ext:<name>: chat with a matching observer may get RunEnded")
	}
}

func TestSettledChatsSkipsLiveChatsAndDedupes(t *testing.T) {
	rep := store.ResumeReport{
		Start:         []store.ResumableNode{{ChatID: "a"}},
		AwaitingInput: []store.ResumableNode{{ChatID: "b"}},
		Failed:        []store.UnresumableNode{{ChatID: "a"}, {ChatID: "c"}, {ChatID: "c"}, {ChatID: ""}},
	}
	if got := settledChats(rep, []string{"b", "d", "c"}); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("settledChats = %v, want [c d]", got)
	}
}

// TestEndSettledExtRunsReportsFailure: a run boot failed instead of resuming reaches its extension as RunFailed.
func TestEndSettledExtRunsReportsFailure(t *testing.T) {
	obs := &outcomeObserver{}
	endSettledExtRuns([]string{"chat-plain", "ext:obs:pr-2"}, []builtSDKExtension{{name: "obs", ext: obs}})
	if len(obs.outcomes) != 1 || obs.outcomes[0].Status != extsdk.RunFailed || obs.outcomes[0].Error != settledRunError {
		t.Fatalf("RunEnded outcomes = %+v, want one RunFailed carrying settledRunError", obs.outcomes)
	}
}

// TestDriveResume_InterruptedSkipsRunEnded: a resume cut by another shutdown is resumed again next boot, so
// finalizing it now would post for unfinished work.
func TestDriveResume_InterruptedSkipsRunEnded(t *testing.T) {
	ctx := context.Background()
	chatID := "ext:obs:pr-interrupted"
	st, orch, plan := newOneNodeResumeFixture(t, chatID)
	obs := &outcomeObserver{}
	hub := stream.NewHub()
	go func() {
		for !hub.HasRegisteredRun(chatID) {
			time.Sleep(time.Millisecond)
		}
		hub.MarkInterrupted(chatID) // DrainActiveRuns' force-cancel
		hub.CancelRun(chatID)
	}()
	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, hub, runlog.NewEventLog(st), runObserverFor(chatID, []builtSDKExtension{{name: "obs", ext: obs}}))
	if len(obs.outcomes) != 0 {
		t.Errorf("RunEnded outcomes = %+v, want none: a shutdown-cut resume must not finalize", obs.outcomes)
	}
}

// TestEndSettledExtRunsSurvivesPanic: one extension's panic neither crashes boot nor skips the other chats.
func TestEndSettledExtRunsSurvivesPanic(t *testing.T) {
	obs := &outcomeObserver{}
	exts := []builtSDKExtension{{name: "bad", ext: &panicObserver{}}, {name: "obs", ext: obs}}
	endSettledExtRuns([]string{"ext:bad:1", "ext:obs:2"}, exts)
	if len(obs.outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want the chat after the panic still delivered", obs.outcomes)
	}
}

type panicObserver struct{ fakeRunObserver }

func (*panicObserver) RunEnded(string, extsdk.RunOutcome) { panic("boom") }

type outcomeObserver struct {
	fakeRunObserver
	outcomes []extsdk.RunOutcome
}

func (o *outcomeObserver) RunEnded(_ string, out extsdk.RunOutcome) {
	o.outcomes = append(o.outcomes, out)
}

// TestDriveResume_ReachesWorkerInOriginalScope: RetryNode's synthetic "chatID::retry" session id must not
// leak into workspace/jail scope - the resumed node must still find the original clone.
func TestDriveResume_ReachesWorkerInOriginalScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	chatID := "ext:github:chat-boot-resume-scope"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	userID := st.SessionUserForChat(ctx, chatID)

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	// The repo setup left at original dispatch, in the REAL chat scope - never re-cloned on resume.
	repoDir, err := jail.EnsureDir(userID, chatID, workspace.NodeDir("n1"))
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("REPO-MARKER"), 0o644); err != nil {
		t.Fatalf("seed README.md: %v", err)
	}

	plan := dag.Plan{ID: "plan-scope", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "reader", Task: "READ-TASK"},
	}}
	planJSON, _ := json.Marshal(plan)
	if err := st.SaveDagPlan(ctx, chatID, plan.ID, "turn-1", string(planJSON)); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, store.DagNode{PlanID: plan.ID, NodeID: "n1", Status: string(dag.StatusPaused), PauseReason: string(dag.PauseShutdown)}); err != nil {
		t.Fatalf("UpsertDagNode n1: %v", err)
	}
	if err := st.SetNodeStatusForChat(ctx, chatID, "n1", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("persist pause: %v", err)
	}

	stub := &resumeStubLLM{}
	sessions := session.InMemoryService()

	readFileTool, err := tools.Build([]string{"read_file"}, tools.Deps{
		Workspace: jail, WorkspaceUserID: userID, WorkspaceCaps: workspace.DefaultCaps(),
		CallScope: tools.CallScope{AdvisorToken: vetting.AdvisorThreadToken(plan.ID, "n1")},
	})
	if err != nil {
		t.Fatalf("tools.Build: %v", err)
	}
	ag, err := llmagent.New(llmagent.Config{Name: "reader", Model: stub, Description: "reader",
		Instruction: "ROLE:reader Read README.md and answer.", Tools: readFileTool})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"reader": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)

	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, stream.NewHub(), runlog.NewEventLog(st), nil)

	n1, err := st.GetDagNode(ctx, plan.ID, "n1")
	if err != nil || n1 == nil {
		t.Fatalf("GetDagNode n1: %v %v", n1, err)
	}
	if !strings.Contains(n1.Output, "REPO-MARKER") {
		t.Fatalf("resumed node's output = %q, want it to contain REPO-MARKER - fs tools did not land in the original chat scope", n1.Output)
	}
}
