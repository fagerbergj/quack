package dag

import (
	"context"
	"iter"
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

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

type askArgs struct {
	Question string `json:"question"`
}
type askResult struct {
	Status string `json:"status"`
}

// newAskTool mirrors tools.NewAskUserTool: a plain tool that records the question
// (in its call args) and ends the worker's turn; the GATE detects the call and
// pauses the node. Built inline to avoid the tools→dag import cycle.
func newAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: vetting.AskToolName, Description: "Ask the user a question."},
		func(tc adkagent.Context, _ askArgs) (askResult, error) {
			tc.Actions().SkipSummarization = true
			return askResult{Status: "forwarded to the user"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// hitlStub: as a judge it passes; as a worker it asks the user via ask_user unless
// its request already carries the delivered answer (the gate's withUserAnswer
// prompt), in which case it writes the final answer.
type hitlStub struct {
	mu          sync.Mutex
	workerCalls int
	sawAnswer   string // the user answer text observed in the post-answer prompt
}

func (*hitlStub) Name() string { return "hitlStub" }

func (s *hitlStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		s.mu.Unlock()
		if txt := gUserText(req); strings.Contains(txt, "they answered") {
			// Post-answer run: extract the answer line for assertions.
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.mu.Lock()
				s.sawAnswer = line
				s.mu.Unlock()
			}
			yield(gText("Final answer using the user's direction."), nil)
			return
		}
		yield(gCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
	}
}

// TestHITL_SingleNodePauseResume covers the degenerate plan where the ASKER is
// itself the terminal node (no synthesizer): run 1 parks it under hitl-n1-r1; the
// answer turn re-enters it and its output becomes the plan's terminal answer.
func TestHITL_SingleNodePauseResume(t *testing.T) {
	stub := &hitlStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	var pauseID, pauseMsg string
	yield := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			pauseID, pauseMsg = d.InterruptID, d.Message
		}
		return true
	}
	out1 := map[string]string{}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s",
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, yield, out1, nil)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !paused || out1["n1"] != "" {
		t.Fatalf("run1: want paused with no output, got paused=%v out=%q", paused, out1["n1"])
	}
	if pauseID != "hitl-n1-r1" || pauseMsg != "which direction?" {
		t.Fatalf("run1: node_needs_input = (%q, %q), want (hitl-n1-r1, which direction?)", pauseID, pauseMsg)
	}

	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: pauseID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	out2 := map[string]string{}
	paused2, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s", answer, yield, out2, []string{"n1"})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if paused2 || out2["n1"] == "" {
		t.Fatalf("run2: want completed with output, got paused=%v out=%q", paused2, out2["n1"])
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawAnswer != "north" {
		t.Errorf("worker never received the user's answer: sawAnswer=%q", stub.sawAnswer)
	}
}

// newChattyAskTool is an ask_user tool that, unlike newAskTool, does NOT set
// SkipSummarization — so after the ask the worker's model gets another turn and
// writes a DRAFT. That reproduces the live-bug STATE the plain ask can't: a
// worker whose RunNode returns a NON-EMPTY draft while a fresh ask_user sits
// unanswered in the session (a chatty code-implementer that asked a real design
// question yet also kept writing). scanNodeAsks keys on the ask_user call name,
// so the gate detects the ask regardless of SkipSummarization; the fix is that
// the pause must fire even though the draft is non-empty.
func newChattyAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: vetting.AskToolName, Description: "Ask the user a question."},
		func(_ adkagent.Context, _ askArgs) (askResult, error) {
			return askResult{Status: "forwarded to the user"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// chattyAskStub asks once, then (given a second turn, because newChattyAskTool
// doesn't end the turn) writes a non-empty draft. On the post-answer run it
// writes the final answer and records the folded answer text.
type chattyAskStub struct {
	mu        sync.Mutex
	calls     int
	sawAnswer string
}

func (*chattyAskStub) Name() string { return "chattyAskStub" }

func (s *chattyAskStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if txt := gUserText(req); strings.Contains(txt, "they answered") {
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.mu.Lock()
				s.sawAnswer = line
				s.mu.Unlock()
			}
			yield(gText("Final answer using the user's direction."), nil)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(gCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
			return
		}
		// Second turn (the model kept going after asking): a NON-EMPTY draft.
		yield(gText("Draft: I'll use Canvas for now, pending your answer."), nil)
	}
}

// TestHITL_PausesDespiteNonEmptyDraft is the regression for the live bug: a
// worker that calls ask_user AND still produces draft text must PAUSE the node
// (the fresh ask isn't dropped just because a draft exists), discarding the
// answer-less draft; the answer turn re-runs the worker with the Q&A folded in.
// Before the fix the non-empty draft masked the fresh ask and sailed to the
// judge, so the question was never answerable.
func TestHITL_PausesDespiteNonEmptyDraft(t *testing.T) {
	stub := &chattyAskStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newChattyAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	var pauseID, pauseMsg string
	yield := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			pauseID, pauseMsg = d.InterruptID, d.Message
		}
		return true
	}
	out1 := map[string]string{}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s",
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, yield, out1, nil)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !paused {
		t.Fatal("run1: worker asked a question with a non-empty draft — the node MUST pause, not proceed to the judge")
	}
	if out1["n1"] != "" {
		t.Errorf("run1: draft leaked as output %q — it should be discarded on the pause", out1["n1"])
	}
	if pauseID != "hitl-n1-r1" || pauseMsg != "which direction?" {
		t.Fatalf("run1: node_needs_input = (%q, %q), want (hitl-n1-r1, which direction?)", pauseID, pauseMsg)
	}

	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: pauseID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "react"},
		},
	}}}
	out2 := map[string]string{}
	paused2, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s", answer, yield, out2, []string{"n1"})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if paused2 || out2["n1"] == "" {
		t.Fatalf("run2: want completion with output, got paused=%v out=%q", paused2, out2["n1"])
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawAnswer != "react" {
		t.Errorf("worker never received the user's answer: sawAnswer=%q", stub.sawAnswer)
	}
}

// multiRoundStub asks TWO questions across two separate pauses before finally
// answering, so the test can assert the round-2+ prompt folds in the FULL
// transcript (both Q&A pairs), not just the latest one.
type multiRoundStub struct {
	mu        sync.Mutex
	finalText string // the full prompt text seen on the final (answering) call
}

func (*multiRoundStub) Name() string { return "multiRoundStub" }

func (s *multiRoundStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		// Route on which answers are present, not on occurrence counts: a scoped
		// single-turn worker's request carries the prompt twice (ADK prepends the
		// node input alongside the seeded user event), so counting "\nA: " would
		// double-count.
		txt := gUserText(req)
		switch {
		case !strings.Contains(txt, "A: north"):
			yield(gCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
		case !strings.Contains(txt, "A: coastal"):
			yield(gCall(vetting.AskToolName, map[string]any{"question": "which region?"}), nil)
		default:
			s.mu.Lock()
			s.finalText = txt
			s.mu.Unlock()
			yield(gText("Final answer using both directions."), nil)
		}
	}
}

// TestHITL_MultiRoundFoldsFullTranscript: a node paused for TWO separate
// questions across two rounds must see BOTH Q&A pairs on its final (answering)
// run — not just the most recent one. Guards the withUserAnswer/hitlScan
// full-transcript fix (a single-pair fold would silently drop round 1's Q&A).
func TestHITL_MultiRoundFoldsFullTranscript(t *testing.T) {
	stub := &multiRoundStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	yield := func(stream.SSEEvent, error) bool { return true }
	run := func(content *genai.Content) (map[string]string, bool) {
		out := map[string]string{}
		paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s", content, yield, out, []string{"n1"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return out, paused
	}
	answerContent := func(interruptID, payload string) *genai.Content {
		return &genai.Content{Role: "user", Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
				Response: map[string]any{"payload": payload},
			},
		}}}
	}

	// Round 1: parks asking "which direction?".
	_, paused := run(&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}})
	if !paused {
		t.Fatal("round 1: expected a pause")
	}
	// Round 2: answer round 1 → parks again asking "which region?".
	_, paused = run(answerContent("hitl-n1-r1", "north"))
	if !paused {
		t.Fatal("round 2: expected a second pause")
	}
	// Round 3: answer round 2 → completes.
	out, paused := run(answerContent("hitl-n1-r2", "coastal"))
	if paused || out["n1"] == "" {
		t.Fatalf("round 3: expected completion, got paused=%v out=%q", paused, out["n1"])
	}

	stub.mu.Lock()
	final := stub.finalText
	stub.mu.Unlock()
	if !strings.Contains(final, "Q: which direction?") || !strings.Contains(final, "A: north") {
		t.Errorf("final prompt missing round 1's Q&A:\n%s", final)
	}
	if !strings.Contains(final, "Q: which region?") || !strings.Contains(final, "A: coastal") {
		t.Errorf("final prompt missing round 2's Q&A:\n%s", final)
	}
}
