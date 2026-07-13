package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// The continuation contract (live TC2 failures 2026-07-13): a worker that ends a
// turn with no answer text is almost always MID-TASK — it spent its output budget
// on reasoning — not done. The gate must CONTINUE it (tools intact, own session)
// rather than hand a tool-less writer the job of summarizing half-finished work.
// The same applies to a worker whose task demanded a commit/push it never made.

type contStub struct {
	workerCalls  int
	writerCalls  int
	judgeCalls   int
	workerTooled []bool   // did each worker request carry the worker's tools?
	workerTexts  []string // the text each worker request carried
}

func (m *contStub) Name() string { return "contStub" }

// GenerateContent routes by request shape: a submit_verdict tool ⇒ the judge; NO
// tools at all ⇒ the tool-less finalize writer; otherwise the worker. The worker
// returns an EMPTY draft first (the failure mode), then — once it sees the
// continuation directive — calls its git_commit tool and writes up what it did.
func (m *contStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		text := stubAllText(req)
		switch {
		case stubHasTool(req, submitVerdictTool):
			m.judgeCalls++
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": "ok"}), nil)
		case !stubHasTool(req, "git_commit"): // tool-less ⇒ the finalize writer
			m.writerCalls++
			yield(stubText("WRITER SUMMARY of half-finished work."), nil)
		default:
			m.workerCalls++
			m.workerTooled = append(m.workerTooled, true)
			m.workerTexts = append(m.workerTexts, text)
			switch {
			case stubHasResponse(req, "git_commit"): // the commit landed
				yield(stubText("Committed and pushed the work."), nil)
			case strings.Contains(text, continuationMarker):
				yield(stubCall("git_commit", map[string]any{"message": "add the game"}), nil)
			default:
				yield(stubText(""), nil) // the empty draft
			}
		}
	}
}

// stubHasResponse reports whether the request carries a tool RESULT for name —
// how a stub model tells "my tool call already ran" from "I still have to make
// it". (Getting this wrong loops the agent forever: ADK's llmagent has no
// iteration cap, so a model that keeps re-calling the same tool never stops.)
func stubHasResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

type commitArgs struct {
	Message string `json:"message"`
}

type commitResult struct {
	Result string `json:"result"`
}

func commitTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[commitArgs, commitResult](
		functiontool.Config{Name: "git_commit", Description: "Commit the staged work."},
		func(_ adkagent.Context, _ commitArgs) (commitResult, error) {
			return commitResult{Result: "git_commit ok"}, nil
		})
	if err != nil {
		t.Fatalf("commit tool: %v", err)
	}
	return tl
}

// runContGate drives the gated node end to end on the real ADK workflow engine,
// returning the final answer plus every gate-authored prompt event it emitted.
func runContGate(t *testing.T, stub model.LLM, cfg Config, task string) (string, []string) {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "implementer",
		Instruction: "Do the task.", Tools: []tool.Tool{commitTool(t)},
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	node, err := newTestGatedNode("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	var final string
	var prompts []string
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: task}}}
	for ev, rerr := range r.Run(t.Context(), "u", "s", content, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
		if ev == nil {
			continue
		}
		if ev.Author == gatePromptAuthor && ev.Content != nil {
			prompts = append(prompts, contentPlainText(ev.Content))
		}
		if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
			final = s
		}
	}
	return final, prompts
}

const implTask = "Add a game to the repo, commit it, push the branch and open a pull request."

// TestEmptyDraft_ContinuesWorkerWithTools: the worker's first turn returns no
// answer. The gate must re-invoke the WORKER (tools intact, own session) with an
// explicit continuation directive delivered as a session event — not summarize
// its half-finished work with the tool-less writer.
func TestEmptyDraft_ContinuesWorkerWithTools(t *testing.T) {
	stub := &contStub{}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", DeliverPromptEvent: true}
	answer, prompts := runContGate(t, stub, cfg, implTask)

	if stub.writerCalls != 0 {
		t.Errorf("tool-less writer ran %d time(s); the worker must be continued instead", stub.writerCalls)
	}
	if !strings.Contains(answer, "Committed and pushed") {
		t.Errorf("answer = %q, want the continued worker's own write-up", answer)
	}
	if stub.workerCalls < 2 {
		t.Fatalf("worker calls = %d, want >=2 (empty draft + continuation)", stub.workerCalls)
	}
	if !stub.workerTooled[1] {
		t.Error("the continuation worker run carried no tools")
	}
	if !strings.Contains(strings.Join(stub.workerTexts[1:], "\n"), continuationMarker) {
		t.Error("the continuation directive never reached the worker's model request")
	}
	// …and it must land as a session event — the only delivery path a remote A2A
	// worker has (it rebuilds its request from session events).
	var sawPromptEvent bool
	for _, p := range prompts {
		if strings.Contains(p, continuationMarker) {
			sawPromptEvent = true
		}
	}
	if !sawPromptEvent {
		t.Error("the continuation prompt was never emitted as a session event")
	}
}

// alwaysEmptyStub: the worker produces nothing, on every turn.
type alwaysEmptyStub struct{ contStub }

func (m *alwaysEmptyStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			m.judgeCalls++
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9}), nil)
		case !stubHasTool(req, "git_commit"):
			m.writerCalls++
			yield(stubText("WRITER SUMMARY from the findings."), nil)
		default:
			m.workerCalls++
			yield(stubText(""), nil)
		}
	}
}

// TestEmptyDraft_FallsBackToWriter: a genuinely stuck worker (empty on every
// continuation) still falls back to the tool-less writer — the existing backstop
// is preserved, just demoted to LAST resort.
func TestEmptyDraft_FallsBackToWriter(t *testing.T) {
	stub := &alwaysEmptyStub{}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10"}
	answer, _ := runContGate(t, stub, cfg, implTask)

	if !strings.Contains(answer, "WRITER SUMMARY") {
		t.Errorf("answer = %q, want the tool-less writer's fallback write-up", answer)
	}
	if stub.workerCalls < 2 {
		t.Errorf("worker calls = %d, want >=2 (the worker must be continued before we give up on it)", stub.workerCalls)
	}
	if stub.writerCalls != 1 {
		t.Errorf("writer calls = %d, want 1 (last resort)", stub.writerCalls)
	}
}

// nonEmptyStub: the worker writes a real draft on its first turn and never calls
// a tool (the v4 shape: a plausible write-up of work it never delivered).
type nonEmptyStub struct{ contStub }

func (m *nonEmptyStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			m.judgeCalls++
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9}), nil)
		case !stubHasTool(req, "git_commit"):
			m.writerCalls++
			yield(stubText("WRITER SUMMARY"), nil)
		default:
			m.workerCalls++
			m.workerTexts = append(m.workerTexts, stubAllText(req))
			yield(stubText("Here is the code I would write. Paris is the capital of France."), nil)
		}
	}
}

// TestNonEmptyDraft_NoContinuation: a worker with a real draft on a task that
// demands no delivery is untouched — one worker run, no continuation, no writer.
func TestNonEmptyDraft_NoContinuation(t *testing.T) {
	stub := &nonEmptyStub{}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10"}
	answer, _ := runContGate(t, stub, cfg, "What is the capital of France?")

	if !strings.Contains(answer, "Paris") {
		t.Errorf("answer = %q, want the worker's own draft", answer)
	}
	if stub.workerCalls != 1 {
		t.Errorf("worker calls = %d, want 1 (draft only — a good draft is never continued)", stub.workerCalls)
	}
	if stub.writerCalls != 0 {
		t.Errorf("writer calls = %d, want 0", stub.writerCalls)
	}
}

// TestUndeliveredDraft_ContinuesWorker: the completion signal is the WORK being
// done, not the model emitting text. A worker that writes a plausible draft for an
// implement-and-deliver task but never committed gets another TOOL-BEARING turn
// (goose-style loop) instead of its half-finished draft sailing to the judge
// (live run v4: judge passed it at 0.7 with zero git_commit calls in the session).
func TestUndeliveredDraft_ContinuesWorker(t *testing.T) {
	stub := &nonEmptyStub{}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10"}
	runContGate(t, stub, cfg, implTask)

	if stub.workerCalls < 2 {
		t.Fatalf("worker calls = %d, want >=2 (an undelivered implement-and-deliver draft must be continued)", stub.workerCalls)
	}
	if !strings.Contains(strings.Join(stub.workerTexts[1:], "\n"), continuationMarker) {
		t.Error("the continuation directive never reached the worker")
	}
}
