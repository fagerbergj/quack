// Package dag_test holds ask_advisor integration tests that need BOTH the
// real dag.Executor/gate machinery AND the real internal/tools.NewAskAdvisorTool
// - internal/tools already imports internal/dag (for dag.Plan/Node and
// dag.NodeTaskStateKey/NodeRubricStateKey), so a same-package (`package dag`)
// test file can't import internal/tools back without a cycle (see
// hitl_test.go's newAskTool comment for the same constraint on ask_user).
// This file, being an EXTERNAL test package, can import both.
package dag_test

import (
	"context"
	"iter"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
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
)

// ── local stub-model helpers (mirrors dag's own gCall/gText/gSysText/gUserText/
// gHasTool - duplicated because those are unexported in `package dag` and this
// is an external test package) ──────────────────────────────────────────────

func atText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func atAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// recordingAdvisor answers every consult with a fixed reply per call number,
// recording the full prompt (including prior session history) it saw -
// the assertion surface for native cross-round/cross-resume session memory.
type recordingAdvisor struct {
	mu      sync.Mutex
	prompts []string
}

func (*recordingAdvisor) Name() string { return "recordingAdvisor" }

func (s *recordingAdvisor) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.prompts = append(s.prompts, atAllText(req))
		n := len(s.prompts)
		s.mu.Unlock()
		yield(atText("ADVICE-"+strconv.Itoa(n)), nil)
	}
}

// newAdvisorTool builds the REAL ask_advisor tool bound to a stub advisor
// agent and sessions, failing the test on construction error.
func newAdvisorTool(t *testing.T, advisorModel model.LLM, sessions session.Service) tool.Tool {
	t.Helper()
	advisorAgent, err := llmagent.New(llmagent.Config{
		Name: "advisor", Model: advisorModel, Description: "advisor", Instruction: "Advise.",
		// ModeChat pinned, as production does via agent.BuildChat: the runner
		// would otherwise force-set it with an unsynchronized write on this
		// SHARED instance - a data race under concurrent consults.
		Mode: llmagent.ModeChat,
	})
	if err != nil {
		t.Fatalf("advisor agent: %v", err)
	}
	tl, err := tools.NewAskAdvisorTool(advisorAgent, sessions)
	if err != nil {
		t.Fatalf("NewAskAdvisorTool: %v", err)
	}
	return tl
}

// runGraphChatID is the chat/session id runGraph runs every plan under. It is
// ALSO the per-chat WORKSPACE scope the run's tools resolve paths through
// (<root>/<user>/<runGraphChatID>/…, derived by tools.chatScopeFromContext), so
// a test seeding a jail fixture for those tools must write it under this SAME
// id - hence one named constant instead of a literal at each site.
const runGraphChatID = "s"

// runGraphNodeID is the id of the single node every runGraph test plan uses. Since
// #198 a node's tools DEFAULT their cwd to the node's OWN dir (<chat>/<node>/), so a
// fixture those tools must act on has to be seeded there - not at the chat root. A
// fixture at the chat root leaves the tool resolving a path that does not exist, and
// a guarded delete then never completes: that is exactly how TestGuardConfirm_OverA2A
// hung for the full 10-minute CI timeout.
const runGraphNodeID = "n1"

// runGraph runs plan via the REAL dag.Executor (RunPlanAsGraph - the native
// graph path production uses), collecting the SSE events and node outputs.
// sessions is shared by BOTH the executor's own runner AND the ask_advisor
// tool baked into worker - exactly how internal/serve wires it (st.Sessions
// passed to both dag.NewExecutor's session.Service and tools.Deps.Sessions).
func runGraph(t *testing.T, worker adkagent.Agent, judgeModel model.LLM, sessions session.Service, plan dag.Plan, content *genai.Content, resumeNodes []string) (paused bool, outputs map[string]string, events []stream.SSEEvent) {
	t.Helper()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": worker}, nil,
		vetting.NewJudgeFactory(judgeModel, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
	outputs = map[string]string{}
	yield := func(ev stream.SSEEvent, _ error) bool { events = append(events, ev); return true }
	p, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", runGraphChatID, content, yield, outputs, resumeNodes)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return p, outputs, events
}

// ── Test 3: memory across gate rounds ───────────────────────────────────────

// draftReviseStub: draft round consults ask_advisor once then writes a draft;
// the judge fails it once (forcing a revision); the revision round consults
// ask_advisor again then writes the final answer. gHasTool routes submit_verdict
// calls to the judge behavior regardless of the worker call counter.
type draftReviseStub struct {
	mu      sync.Mutex
	calls   int
	judged  int
	reqText []string // full request text per worker call (debug/assertions)
}

func (*draftReviseStub) Name() string { return "draftReviseStub" }

func (s *draftReviseStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judged++
			n := s.judged
			s.mu.Unlock()
			if n == 1 {
				yield(atCall("submit_verdict", map[string]any{"score": 0.3, "feedback": "needs work"}), nil)
			} else {
				yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			}
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.reqText = append(s.reqText, atAllText(req))
		s.mu.Unlock()
		switch n {
		case 1:
			yield(atCall("ask_advisor", map[string]any{"request": "DRAFT-REQUEST: how should I scope this?"}), nil)
		case 2:
			yield(atText("draft answer"), nil)
		case 3:
			yield(atCall("ask_advisor", map[string]any{"request": "REVISION-REQUEST: what should I fix?"}), nil)
		default:
			yield(atText("revised answer"), nil)
		}
	}
}

// TestAskAdvisor_MemoryAcrossGateRounds: a consult during a judge-fail
// revision must see the DRAFT round's own consultation (request + reply) -
// native session memory persisting across the gate's draft → revise loop
// within one node invocation, replacing the dropped
// TestAdvisor_RevisionConsultSeesItsOwnPriorAdvice (the gate no longer
// threads advice itself; the worker's own ask_advisor session now does).
func TestAskAdvisor_MemoryAcrossGateRounds(t *testing.T) {
	stub := &draftReviseStub{}
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	tl := newAdvisorTool(t, advisor, sessions)
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{tl},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "do it", Rubric: "must be thorough"},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	paused, outputs, _ := runGraph(t, worker, stub, sessions, plan, content, nil)
	if paused {
		t.Fatal("run should not pause")
	}
	if outputs["n1"] != "revised answer" {
		t.Fatalf("n1 output = %q, want the revision's answer", outputs["n1"])
	}

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.prompts) != 2 {
		t.Fatalf("advisor called %d times, want 2 (draft + revision)", len(advisor.prompts))
	}
	revisionPrompt := advisor.prompts[1]
	if !strings.Contains(revisionPrompt, "DRAFT-REQUEST") {
		t.Errorf("revision consult missing the draft round's request; got:\n%s", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "ADVICE-1") {
		t.Errorf("revision consult missing the advisor's OWN draft-round reply (no native memory); got:\n%s", revisionPrompt)
	}
}

// TestAskAdvisor_StreamsAsToolCall (test case 7): a consultation surfaces as
// an ordinary agent_tool_call/agent_tool_result pair within the worker's own
// run - NOT a separate stage - with the request visible in Args, and the
// advice visible in the result.
func TestAskAdvisor_StreamsAsToolCall(t *testing.T) {
	stub := &draftReviseStub{}
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	tl := newAdvisorTool(t, advisor, sessions)
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{tl},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "do it", Rubric: "must be thorough"},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	_, _, events := runGraph(t, worker, stub, sessions, plan, content, nil)

	var sawDraftRequest, sawResult bool
	for _, ev := range events {
		if ev.Name == stream.EventAgentToolCall {
			if d, ok := ev.Data.(stream.AgentToolCallData); ok && d.Name == "ask_advisor" {
				if req, _ := d.Args["request"].(string); strings.Contains(req, "DRAFT-REQUEST") {
					sawDraftRequest = true
				}
			}
		}
		if ev.Name == stream.EventAgentToolResult {
			if d, ok := ev.Data.(stream.AgentToolResultData); ok && d.Name == "ask_advisor" {
				sawResult = true
			}
		}
		// ask_advisor must never be a separate agent_start stage.
		if ev.Name == stream.EventAgentStart {
			if d, ok := ev.Data.(stream.AgentStartData); ok && d.Agent == "ask_advisor" {
				t.Errorf("ask_advisor surfaced as its own agent_start run (stage=%q) - it must be ordinary tool activity", d.Stage)
			}
		}
	}
	if !sawDraftRequest {
		t.Error("no agent_tool_call for ask_advisor carrying the worker's request text - consultation not visible in the stream")
	}
	if !sawResult {
		t.Error("no agent_tool_result for ask_advisor")
	}
}

// ── Test 4: memory across HITL pause/resume ─────────────────────────────────

// hitlAdvisorStub: consults ask_advisor once before asking the user a
// question (pausing the node); after resume, consults ask_advisor AGAIN
// before answering. gHasTool routes submit_verdict (post-resume judge pass).
type hitlAdvisorStub struct {
	mu    sync.Mutex
	calls int
}

func (*hitlAdvisorStub) Name() string { return "hitlAdvisorStub" }

func (s *hitlAdvisorStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		switch n {
		case 1:
			yield(atCall("ask_advisor", map[string]any{"request": "PRE-PAUSE-REQUEST: am I on the right track?"}), nil)
		case 2:
			yield(atCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
		case 3:
			yield(atCall("ask_advisor", map[string]any{"request": "POST-RESUME-REQUEST: given their answer, what now?"}), nil)
		default:
			yield(atText("final answer using their direction"), nil)
		}
	}
}

// newAskUserTool mirrors tools.NewAskUserTool inline (same tools→dag import-
// cycle constraint as hitl_test.go's newAskTool - but here it's actually
// avoidable since this file is package dag_test and COULD import
// internal/tools; kept inline anyway for symmetry with the rest of this
// package's HITL tests and to keep this test's tool surface minimal/explicit).
func newAskUserTool(t *testing.T) tool.Tool {
	t.Helper()
	type askArgs struct {
		Question string `json:"question"`
	}
	type askResult struct {
		Status string `json:"status"`
	}
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: vetting.AskToolName, Description: "Ask the user a question."},
		func(tc adkagent.Context, _ askArgs) (askResult, error) {
			tc.Actions().SkipSummarization = true
			return askResult{Status: "forwarded to the user"}, nil
		})
	if err != nil {
		t.Fatalf("ask_user tool: %v", err)
	}
	return tl
}

// TestAskAdvisor_MemoryAcrossHITLPauseResume: a post-resume consult must see
// the PRE-PAUSE consultation (same session key - the per-node advisor session
// is keyed by invocation + node, and ADK reuses the paused invocation's ID on
// resume - see NewAskAdvisorTool's doc comment).
func TestAskAdvisor_MemoryAcrossHITLPauseResume(t *testing.T) {
	stub := &hitlAdvisorStub{}
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	advisorTool := newAdvisorTool(t, advisor, sessions)
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{advisorTool, newAskUserTool(t)},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "do it", Rubric: "must be thorough"},
	}}

	// Run 1: parks on ask_user after the pre-pause consult.
	paused1, out1, events1 := runGraph(t, worker, stub, sessions, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused1 || out1["n1"] != "" {
		t.Fatalf("run1: want paused with no output, got paused=%v out=%q", paused1, out1["n1"])
	}
	var interruptID string
	for _, ev := range events1 {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			interruptID = d.InterruptID
		}
	}
	if interruptID == "" {
		t.Fatal("run1: no node_needs_input event")
	}

	// Run 2 (resume): delivers the user's answer; the worker consults
	// ask_advisor again before its final answer.
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	paused2, out2, _ := runGraph(t, worker, stub, sessions, plan, answer, []string{"n1"})
	if paused2 || out2["n1"] == "" {
		t.Fatalf("run2: want completed with output, got paused=%v out=%q", paused2, out2["n1"])
	}

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.prompts) != 2 {
		t.Fatalf("advisor called %d times, want 2 (pre-pause + post-resume)", len(advisor.prompts))
	}
	postResumePrompt := advisor.prompts[1]
	if !strings.Contains(postResumePrompt, "PRE-PAUSE-REQUEST") {
		t.Errorf("post-resume consult missing the pre-pause request; got:\n%s", postResumePrompt)
	}
	if !strings.Contains(postResumePrompt, "ADVICE-1") {
		t.Errorf("post-resume consult missing the advisor's OWN pre-pause reply (session key not stable across resume); got:\n%s", postResumePrompt)
	}
}

// ── A2A repro: production serves workers over A2A ───────────────────────────

// TestAskAdvisor_OverA2A reproduces the LIVE failure (2026-07-09 22:29): in
// production the worker is an A2A remote agent (internal/agent.Serve →
// srv.Client()), so ask_advisor's handler executes inside the A2A SERVER's
// runner - a different session (the A2A context session, AppName = the agent
// name), whose events carry no NodeInfo and whose state has no gate-seeded
// keys. Any identity mechanism that reads the calling runner's session/state/
// path fails there deterministically. This test runs the full production
// shape: gated node → A2A client → loopback A2A server → worker llmagent with
// the REAL ask_advisor tool → judge-fail revision → second consult, and
// asserts the mentor ran, was seeded, and remembered across rounds.
func TestAskAdvisor_OverA2A(t *testing.T) {
	stub := &draftReviseStub{}
	advisor := &recordingAdvisor{}
	// The REAL database-backed session service (sqlite dialect of the same ADK
	// service Postgres uses in production) - so this covers the full production
	// shape: durable DB sessions + the A2A hop. The 2026-07-09 live failure
	// only reproduced with both.
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sessions := st.Sessions
	tl := newAdvisorTool(t, advisor, sessions)
	worker, werr := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{tl},
	})
	if werr != nil {
		t.Fatalf("worker agent: %v", werr)
	}
	// Serve the worker over REAL loopback A2A with the SAME shared session
	// service production uses for everything (internal/serve passes st.Sessions
	// to agent.Serve, the executor, and tools.Deps.Sessions alike).
	srv, err := quackagent.Serve(worker, sessions, nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	defer srv.Close()
	client, err := srv.Client()
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "do it", Rubric: "must be thorough"},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	paused, outputs, _ := runGraph(t, client, stub, sessions, plan, content, nil)
	if paused {
		t.Fatal("run should not pause")
	}
	if !strings.Contains(outputs["n1"], "revised answer") {
		t.Fatalf("n1 output = %q, want the revision's answer", outputs["n1"])
	}
	// The remote worker must actually RECEIVE its node task: remoteagent builds
	// its outbound message from session events only (RunNode input/UserContent
	// is dropped), so without the gate's prompt-delivery event (vetting.
	// emitPrompt) an A2A worker never sees its task at all.
	stub.mu.Lock()
	if len(stub.reqText) == 0 || !strings.Contains(stub.reqText[0], "do it") {
		var first string
		if len(stub.reqText) > 0 {
			first = stub.reqText[0]
		}
		stub.mu.Unlock()
		t.Fatalf("A2A worker's first request missing its node task (prompt not delivered over A2A); got:\n%s", first)
	}
	stub.mu.Unlock()

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.prompts) != 2 {
		t.Fatalf("advisor called %d times over A2A, want 2 (draft + revision) - node identity failed on the A2A server side?", len(advisor.prompts))
	}
	if !strings.Contains(advisor.prompts[0], "do it") {
		t.Errorf("A2A: advisor's 1st request missing the node task seed; got:\n%s", advisor.prompts[0])
	}
	if !strings.Contains(advisor.prompts[1], "DRAFT-REQUEST") || !strings.Contains(advisor.prompts[1], "ADVICE-1") {
		t.Errorf("A2A: revision consult missing the draft round's consultation (no memory across rounds); got:\n%s", advisor.prompts[1])
	}
}

// ── concurrency: same agent, two nodes, isolated mentor threads ─────────────

// concConsultStub is one node's worker model: it consults ask_advisor twice
// (distinct per-node request markers), then answers. One INSTANCE per node -
// identifying the node from request TEXT is unreliable under concurrency
// (co-located single-turn workers share the session, and ADK anchors their
// "current turn" at the latest user-ROLE event, which a concurrent node's
// tool response can hijack), so identity is fixed per instance instead.
// The judge (submit_verdict) always passes. Safe for concurrent calls.
type concConsultStub struct {
	letter string
	mu     sync.Mutex
	calls  int
}

func (s *concConsultStub) Name() string { return "concConsultStub" + s.letter }

func (s *concConsultStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		switch n {
		case 1:
			yield(atCall("ask_advisor", map[string]any{"request": "REQ-" + s.letter + "-1"}), nil)
		case 2:
			yield(atCall("ask_advisor", map[string]any{"request": "REQ-" + s.letter + "-2"}), nil)
		default:
			yield(atText("answer-"+s.letter), nil)
		}
	}
}

// TestAskAdvisor_ConcurrentNodesIsolatedThreads: two nodes of the SAME agent
// run concurrently (maxActive=2) and each consults its mentor twice. Each
// node's SECOND consult must see its OWN first request and never the other
// node's - i.e. the two mentor conversations are distinct advisor sessions
// (the per-node thread token keys them; a shared or misrouted session would
// leak REQ-A-* into B's prompt or vice versa).
func TestAskAdvisor_ConcurrentNodesIsolatedThreads(t *testing.T) {
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	// ONE advisor + ONE tool instance shared by both workers - the advisor-
	// session isolation under concurrency is exactly what's under test. The
	// workers themselves are separate llmagent instances per node: ADK's
	// RunLLMAgentAsNode mutates unsynchronized per-agent state (Mode/
	// IncludeContents, llm_agent_wrapper.go), so a SHARED local llmagent
	// across concurrent nodes races in ADK itself - a test-only hazard;
	// production workers are A2A remote agents with no such state.
	tl := newAdvisorTool(t, advisor, sessions)
	mk := func(name string, m model.LLM) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{
			Name: name, Model: m, Description: name, Instruction: "ROLE:blk Answer.",
			Tools: []tool.Tool{tl},
		})
		if err != nil {
			t.Fatalf("worker agent %s: %v", name, err)
		}
		return a
	}
	stubA, stubB := &concConsultStub{letter: "A"}, &concConsultStub{letter: "B"}
	// The synthesizer consults nothing (its stub starts past the consult calls).
	stubS := &concConsultStub{letter: "S", calls: 2}
	agents := map[string]adkagent.Agent{"blk-a": mk("blk-a", stubA), "blk-b": mk("blk-b", stubB), "blk-s": mk("blk-s", stubS)}
	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "na", AgentName: "blk-a", Task: "TASK-A", Rubric: "ra"},
		{ID: "nb", AgentName: "blk-b", Task: "TASK-B", Rubric: "rb"},
		{ID: "synth", AgentName: "blk-s", Task: "synth", DependsOn: []string{"na", "nb"}},
	}}
	ex := dag.NewExecutor(sessions, agents, nil,
		vetting.NewJudgeFactory(stubA, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "s", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if paused {
		t.Fatal("run should not pause")
	}
	if outputs["na"] == "" || outputs["nb"] == "" {
		t.Fatalf("outputs incomplete: %v", outputs)
	}

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	// 2 consults per researcher node (the synth's task contains both letters and
	// routes as "B"; its calls land in B's counter AFTER B answered, so it never
	// consults - calls 3+ answer immediately).
	var secondA, secondB string
	for _, pr := range advisor.prompts {
		if strings.Contains(pr, "REQ-A-2") {
			secondA = pr
		}
		if strings.Contains(pr, "REQ-B-2") {
			secondB = pr
		}
	}
	if secondA == "" || secondB == "" {
		t.Fatalf("missing a second consult: %d advisor prompts", len(advisor.prompts))
	}
	if !strings.Contains(secondA, "REQ-A-1") {
		t.Errorf("node A's 2nd consult missing its OWN 1st request:\n%s", secondA)
	}
	if strings.Contains(secondA, "REQ-B-1") || strings.Contains(secondA, "TASK-B") && !strings.Contains(secondA, "TASK-A") {
		t.Errorf("node A's mentor thread contaminated by node B:\n%s", secondA)
	}
	if !strings.Contains(secondB, "REQ-B-1") {
		t.Errorf("node B's 2nd consult missing its OWN 1st request:\n%s", secondB)
	}
	if strings.Contains(secondB, "REQ-A-1") {
		t.Errorf("node B's mentor thread contaminated by node A:\n%s", secondB)
	}
	if strings.Contains(secondA, "REQ-B-2") || strings.Contains(secondB, "REQ-A-2") {
		t.Error("mentor threads interleaved across concurrent nodes")
	}
}
