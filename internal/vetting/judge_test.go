package vetting

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

// spyReadArgs/spyReadResult mirror read_file's minimal shape for a stub tool
// that records it was called and returns a scripted file body.
type spyReadArgs struct {
	Path string `json:"path"`
}
type spyReadResult struct {
	Content string `json:"content"`
}

// newSpyReadTool returns a stand-in read_file tool that returns body and bumps
// calls each time the judge invokes it - proving the judge actually opened the
// file before scoring, without needing a real jail.
func newSpyReadTool(t *testing.T, body string, calls *int32) tool.Tool {
	t.Helper()
	rt, err := functiontool.New[spyReadArgs, spyReadResult](
		functiontool.Config{Name: "read_file", Description: "Read a text file from your workspace."},
		func(_ adkagent.Context, _ spyReadArgs) (spyReadResult, error) {
			atomic.AddInt32(calls, 1)
			return spyReadResult{Content: body}, nil
		},
	)
	if err != nil {
		t.Fatalf("spy read tool: %v", err)
	}
	return rt
}

// readFileResponseContent extracts the content a prior read_file tool call
// returned into the judge's request, so the scripted judge model can react to
// the file body it "read".
func readFileResponseContent(req *model.LLMRequest) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "read_file" {
				if v, ok := p.FunctionResponse.Response["content"].(string); ok {
					return v, true
				}
				return "", true
			}
		}
	}
	return "", false
}

// scriptedJudge is a deterministic judge model: it first calls read_file for
// the changed file, then - once it has the body back - submits a verdict whose
// score is DERIVED from the file's contents (pass iff it contains a test). This
// proves the agentic read loop grounds the score in the real source.
type scriptedJudge struct{}

func (scriptedJudge) Name() string { return "scripted-judge" }

func (scriptedJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if content, seen := readFileResponseContent(req); seen {
			score := 0.2
			if strings.Contains(content, "func Test") {
				score = 0.9
			}
			yield(stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "graded from the file"}), nil)
			return
		}
		yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
	}
}

// TestJudgeReadsFileBeforeVerdict drives the agentic judge with a read tool and
// asserts it OPENS the file before scoring and that the verdict reflects the
// file's contents: a file missing its test fails; the same file with a test
// passes.
func TestJudgeReadsFileBeforeVerdict(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantScore float64
	}{
		{"missing test fails", "func Play() {}\n", 0.2},
		{"has test passes", "func Play() {}\nfunc TestPlay(t *testing.T) {}\n", 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			readTool := newSpyReadTool(t, tc.body, &calls)
			factory := NewJudgeFactory(scriptedJudge{}, []tool.Tool{readTool}, nil)
			q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
			v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
				"I implemented game.go", workerActivity{}, nil, func(*genai.Part) bool { return true })
			if err != nil {
				t.Fatalf("runJudgeAgent: %v", err)
			}
			if atomic.LoadInt32(&calls) != 1 {
				t.Errorf("read_file calls = %d, want 1 (judge must open the file)", calls)
			}
			if v.Score != tc.wantScore {
				t.Errorf("verdict score = %v, want %v (should reflect the file body)", v.Score, tc.wantScore)
			}
		})
	}
}

// recordingJudge captures the full text of every judge prompt it receives
// (into *prompt) and always submits a fixed-score verdict - a stand-in for
// asserting what the ASSEMBLED judge prompt looked like, not what the judge
// decided.
type recordingJudge struct{ prompt *string }

func (recordingJudge) Name() string { return "recording-judge" }

func (r recordingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		*r.prompt = stubAllText(req)
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil)
	}
}

// TestRunJudgeAgent_OverBudgetAnswerFitsBudget proves issue #291's budgeting
// fix: an answer big enough that the assembled judge prompt would blow past
// the judge model's configured context window gets clamped BEFORE the call
// (fitJudgeAnswer), so the judge still sees a within-budget prompt and
// produces a verdict instead of the call 400ing against the model's slot.
func TestRunJudgeAgent_OverBudgetAnswerFitsBudget(t *testing.T) {
	var seenPrompt string
	factory := NewJudgeFactory(recordingJudge{prompt: &seenPrompt}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	// Far larger than any real judge slot (~125k tokens raw) - the same shape as
	// the #291 incident's 34K-token judge call against a 32K/64K model slot.
	hugeAnswer := strings.Repeat("the worker wrote a very long answer. ", 15_000)
	cfg := Config{Rubric: "score 0-10", JudgeContextWindow: 8_000} // small window forces a real clamp

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, hugeAnswer, workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.8 {
		t.Errorf("verdict score = %v, want 0.8 (the judge call should have completed)", v.Score)
	}

	budget := judgeCharBudget(cfg)
	// stubAllText joins content parts with a trailing "\n" per part (test helper
	// artifact, not part of the actual request) - trim it before comparing.
	seenPrompt = strings.TrimRight(seenPrompt, "\n")
	if len(seenPrompt) > budget {
		t.Errorf("judge saw a %d-char prompt, exceeds the %d-char budget derived from JudgeContextWindow - the oversized answer was not clamped", len(seenPrompt), budget)
	}
	if len(seenPrompt) >= len(hugeAnswer) {
		t.Errorf("judge prompt (%d chars) is not smaller than the raw answer (%d chars) - expected compaction", len(seenPrompt), len(hugeAnswer))
	}
}

// TestRunJudgeAgent_SessionIDIsChatIDNotConstant is the Langfuse-attribution
// regression: ADK's runner.Run takes the session id as its third argument and
// stamps gen_ai.conversation.id from it (google.golang.org/adk/v2/internal/
// telemetry), so a hardcoded "verdict" literal collapsed every judge call
// ever made, across every chat, into one Langfuse session. A tool callback
// sees the real ADK session id via adkagent.Context.SessionID() - assert that
// runJudgeRound passed cfg.ChatID, not the old constant.
func TestRunJudgeAgent_SessionIDIsChatIDNotConstant(t *testing.T) {
	var gotSessionID string
	spy, err := functiontool.New[spyReadArgs, spyReadResult](
		functiontool.Config{Name: "read_file", Description: "Read a text file from your workspace."},
		func(tc adkagent.Context, _ spyReadArgs) (spyReadResult, error) {
			gotSessionID = tc.SessionID()
			return spyReadResult{Content: "func Play() {}\nfunc TestPlay(t *testing.T) {}\n"}, nil
		},
	)
	if err != nil {
		t.Fatalf("spy tool: %v", err)
	}
	factory := NewJudgeFactory(scriptedJudge{}, []tool.Tool{spy}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
	cfg := Config{Rubric: "score 0-10", ChatID: "chat-42"}

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "I implemented game.go", workerActivity{}, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if gotSessionID != "chat-42" {
		t.Errorf("judge run session id = %q, want %q (the chat id, not the old \"verdict\" constant)", gotSessionID, "chat-42")
	}
}

// flakyTransientJudge fails its first `failures` calls with a transient-
// looking error (a 502, standing in for a model swap in flight), then submits
// a normal verdict - the stand-in for #572's incident.
type flakyTransientJudge struct {
	failures int32
	calls    int32
}

func (j *flakyTransientJudge) Name() string { return "flaky-transient-judge" }

func (j *flakyTransientJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atomic.AddInt32(&j.calls, 1) <= atomic.LoadInt32(&j.failures) {
			yield(nil, errors.New("openai gemma4-26b-a4b (generate): status 502: bad gateway"))
			return
		}
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.85, "feedback": "recovered"}), nil)
	}
}

// TestRunJudgeAgent_RetriesTransientErrorThenSucceeds proves #572's fix: a
// judge call that fails with a transient-looking error (502) is retried with
// backoff and, once the endpoint recovers, produces a NORMAL scored verdict -
// never a degrade.
func TestRunJudgeAgent_RetriesTransientErrorThenSucceeds(t *testing.T) {
	judge := &flakyTransientJudge{failures: 2}
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.85 {
		t.Errorf("verdict score = %v, want 0.85 (the retry that finally recovered)", v.Score)
	}
	if got := atomic.LoadInt32(&judge.calls); got != 3 {
		t.Errorf("judge model called %d times, want 3 (two transient 502s + the recovering call)", got)
	}
}

// TestRunJudgeAgent_PermanentTransientErrorFailsClosed proves the other half:
// a judge that never recovers within judgeRetryAttempts still returns an
// error (fail closed), not a silent pass - node.go's caller is what turns
// this into a visible caveat rather than a stripped verdict.
func TestRunJudgeAgent_PermanentTransientErrorFailsClosed(t *testing.T) {
	judge := &flakyTransientJudge{failures: 100} // never recovers
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	_, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err == nil {
		t.Fatal("runJudgeAgent: expected an error when the judge model never recovers, got nil")
	}
	if got := atomic.LoadInt32(&judge.calls); got != judgeRetryAttempts {
		t.Errorf("judge model called %d times, want %d (judgeRetryAttempts; the short answer leaves the shrink-retry a no-op)", got, judgeRetryAttempts)
	}
}

// TestIsTransientJudgeErr pins the retry predicate: only endpoint-fault-
// shaped errors are worth a backoff retry, never a genuine rejection that
// retrying would just repeat.
func TestIsTransientJudgeErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"502", errors.New("openai model (generate): status 502: bad gateway"), true},
		{"503", errors.New("openai model (generate): status 503: unavailable"), true},
		{"429", errors.New("openai model (generate): status 429: rate limited"), true},
		{"timeout", errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"deadline exceeded sentinel", context.DeadlineExceeded, true},
		{"400 bad request", errors.New("openai model (generate): status 400: context length exceeded"), false},
		{"401 auth", errors.New("openai model (generate): status 401: invalid api key"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientJudgeErr(tt.err); got != tt.want {
				t.Errorf("isTransientJudgeErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// fakeToolset is a minimal tool.Toolset exposing a single scripted skill tool,
// standing in for the real skilltoolset so the skill-loading path is hermetic.
type fakeToolset struct{ tools []tool.Tool }

func (f fakeToolset) Name() string { return "fake-skills" }
func (f fakeToolset) Tools(_ adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return f.tools, nil
}

// skillLoadArgs/skillLoadResult mirror load_skill's minimal shape.
type skillLoadArgs struct {
	Name string `json:"name"`
}
type skillLoadResult struct {
	Instructions string `json:"instructions"`
}

// newSpyLoadSkillTool returns a stand-in load_skill tool that returns body and
// bumps calls each time the judge invokes it - proving the skill toolset reaches
// the judge and is callable before submit_verdict.
func newSpyLoadSkillTool(t *testing.T, body string, calls *int32) tool.Tool {
	t.Helper()
	lt, err := functiontool.New[skillLoadArgs, skillLoadResult](
		functiontool.Config{Name: "load_skill", Description: "Load a skill's full instructions before applying it."},
		func(_ adkagent.Context, _ skillLoadArgs) (skillLoadResult, error) {
			atomic.AddInt32(calls, 1)
			return skillLoadResult{Instructions: body}, nil
		},
	)
	if err != nil {
		t.Fatalf("spy load_skill tool: %v", err)
	}
	return lt
}

// skillResponseContent extracts the instructions a prior load_skill call
// returned into the judge's request, so the scripted judge can react to them.
func skillResponseContent(req *model.LLMRequest) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "load_skill" {
				if v, ok := p.FunctionResponse.Response["instructions"].(string); ok {
					return v, true
				}
				return "", true
			}
		}
	}
	return "", false
}

// skillJudge first loads a review skill, then - once it has the skill's
// instructions back - submits a verdict whose score is DERIVED from them (pass
// iff the skill mandates a test). This proves the judge grounds its score in a
// skill it loaded agentically, using the same skill library the worker had.
type skillJudge struct{}

func (skillJudge) Name() string { return "skill-judge" }

func (skillJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if instr, seen := skillResponseContent(req); seen {
			score := 0.3
			if strings.Contains(instr, "require a test") {
				score = 0.9
			}
			yield(stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "graded against the loaded skill"}), nil)
			return
		}
		yield(stubCall("load_skill", map[string]any{"name": "ponytail-review"}), nil)
	}
}

// TestJudgeLoadsSkillBeforeVerdict proves the skill toolset reaches the judge
// and is callable: the judge loads a review skill, then scores against its
// principles (a skill that mandates tests yields a higher score than one that
// does not) before calling submit_verdict.
func TestJudgeLoadsSkillBeforeVerdict(t *testing.T) {
	cases := []struct {
		name      string
		skillBody string
		wantScore float64
	}{
		{"skill without test mandate", "review for clarity", 0.3},
		{"skill mandates a test", "review code and require a test for every change", 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			skillTool := newSpyLoadSkillTool(t, tc.skillBody, &calls)
			ts := fakeToolset{tools: []tool.Tool{skillTool}}
			factory := NewJudgeFactory(skillJudge{}, nil, []tool.Toolset{ts})
			q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
			v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
				"I implemented game.go", workerActivity{}, nil, func(*genai.Part) bool { return true })
			if err != nil {
				t.Fatalf("runJudgeAgent: %v", err)
			}
			if atomic.LoadInt32(&calls) != 1 {
				t.Errorf("load_skill calls = %d, want 1 (judge must load the skill)", calls)
			}
			if v.Score != tc.wantScore {
				t.Errorf("verdict score = %v, want %v (should reflect the loaded skill)", v.Score, tc.wantScore)
			}
		})
	}
}

// oneShotJudge submits a verdict on the first turn without calling any tool -
// the pure-research, no-read-tools path.
type oneShotJudge struct{}

func (oneShotJudge) Name() string { return "one-shot-judge" }

func (oneShotJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil)
	}
}

// TestJudgeNoReadToolsOneShot verifies the factory still builds a working
// one-shot judge when no read tools are supplied (backward compat / research
// deployments with no workspace jail).
func TestJudgeNoReadToolsOneShot(t *testing.T) {
	factory := NewJudgeFactory(oneShotJudge{}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
		"Paris.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.8 {
		t.Errorf("verdict score = %v, want 0.8", v.Score)
	}
}

// TestJudgeBehaviourSelectsClause pins the prompt-clause selection: the read-
// tools clause appears only when the judge holds read tools, and the no-tools
// clause only when it does not.
func TestJudgeBehaviourSelectsClause(t *testing.T) {
	with := judgeBehaviour(true, false)
	if !strings.Contains(with, "read-only workspace tools") || strings.Contains(with, "You have no tools") {
		t.Errorf("read-tools behaviour missing its clause: %q", with)
	}
	// #502/#498: the judge must be told the clone root is its working root and
	// never to use a leading-slash/absolute path - the worker gets this same
	// grounding, and its absence sent a judge into a dead-end "/frontend" retry
	// loop until the repeat-guard gave up (a silent gate bypass).
	if !strings.Contains(with, "plain repo-relative paths") || !strings.Contains(with, "NEVER use a leading slash") {
		t.Errorf("read-tools behaviour missing repo-relative path grounding: %q", with)
	}
	without := judgeBehaviour(false, false)
	if !strings.Contains(without, "You have no tools") || strings.Contains(without, "read-only workspace tools") {
		t.Errorf("no-tools behaviour missing its clause: %q", without)
	}
	// The skills clause appears only when the judge holds the skill toolset.
	withSkills := judgeBehaviour(false, true)
	if !strings.Contains(withSkills, "skill tools") || !strings.Contains(withSkills, "load a relevant") {
		t.Errorf("with-skills behaviour missing its clause: %q", withSkills)
	}
	if strings.Contains(without, "skill tools") {
		t.Errorf("no-skills behaviour must not mention skill tools: %q", without)
	}
}

// TestJudgePromptScopedToNodeNotOrchestratorFileCount pins #664's test case
// 3: the judge is handed exactly what the node it judges saw (nodeTask, which
// after the consumer split carries the node's own scoped ask+task, never the
// orchestrator's <changed_files count=...> summary) plus changedFiles sourced
// from the actual clone diff (buildImplementDiffSection/buildChangedFilesSection,
// verified by inspection - neither reads plan.UserMessage or any orchestrator
// count). The judge prompt must reflect the real diff, and must not manufacture
// or otherwise surface an orchestrator-style file count it was never given.
func TestJudgePromptScopedToNodeNotOrchestratorFileCount(t *testing.T) {
	nodeTask := "<permissions>push_commits_to_pr</permissions>\n<deliverable>a commit</deliverable>\n" +
		"<issue number=\"7\"><title>t</title><description>d</description></issue>\n\n" +
		"YOUR TASK - do this, and ONLY this:\nFix the failing build check in internal/foo.go."
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "irrelevant outer question"}}}
	changedFiles := "diff --git a/internal/foo.go b/internal/foo.go\n--- a/internal/foo.go\n+++ b/internal/foo.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"

	prompt := buildJudgePrompt("", "rubric text", nodeTask, question, "answer text", changedFiles, workerActivity{}, "")

	if !strings.Contains(prompt, changedFiles) {
		t.Errorf("judge prompt missing the actual diff content:\n%s", prompt)
	}
	if strings.Contains(prompt, `<changed_files count=`) {
		t.Errorf("judge prompt must never surface an orchestrator-style file count it was never given:\n%s", prompt)
	}
	if !strings.Contains(prompt, nodeTask) {
		t.Errorf("judge prompt must score against exactly the node's own task, verbatim:\n%s", prompt)
	}
}

// TestBuildJudgePromptSectionOrder pins the cache-friendly section order:
// the stable, round-invariant sections (constitution, rubric, task,
// question, answer) come before the volatile, re-derived-per-round evidence
// (ledger, changed files, known failures) - so the prefix up to and
// including the answer stays byte-identical, and a cache hit, across rounds.
func TestBuildJudgePromptSectionOrder(t *testing.T) {
	act := workerActivity{workspace: []wsOp{{tool: "read_file", detail: `read_file(path="README.md")`}}}
	det := map[string]criterionScore{"checks_pass": {Score: 0, Reason: "deterministic: build failed"}}
	known := judgeKnownFailuresSection(det, 0.7)

	prompt := buildJudgePrompt("the constitution", "the rubric", "the node task",
		questionContent("the question"), "the answer", "the changed files diff", act, known)

	sections := []string{"the constitution", "the rubric", "the node task", "the question", "the answer",
		"Workspace activity", "the changed files diff", judgeKnownFailuresHeader}
	last := -1
	for _, s := range sections {
		idx := strings.Index(prompt, s)
		if idx < 0 {
			t.Fatalf("prompt missing section %q:\n%s", s, prompt)
		}
		if idx < last {
			t.Fatalf("section %q is out of order (want constitution, rubric, task, question, answer, ledger, changed files, known failures):\n%s", s, prompt)
		}
		last = idx
	}
}

// TestBuildJudgePromptStablePrefixIsByteIdentical pins what the section-order
// test above cannot: two rounds of the SAME node must produce prompts that are
// byte-identical up to where the answer body starts. Order alone is not enough -
// a clock, a run id, or a re-derived path leaking into the constitution/rubric/
// task/question would keep every section in place and still move the first
// differing byte to offset 0, which is what llama.cpp's per-slot prefix cache
// actually measures (verified against the production judge: two rounds assembled
// this way reuse 2,899 tokens; a prompt that diverges at byte 0 reuses none).
func TestBuildJudgePromptStablePrefixIsByteIdentical(t *testing.T) {
	const constitution, rubric, task = "the constitution", "the rubric", "the node task"
	question := questionContent("the question")

	// Round 1 and round 2 of one node: same stable head, different answer and
	// different volatile evidence (ledger, changed files, known failures).
	first := buildJudgePrompt(constitution, rubric, task, question, "the first answer", "diff one",
		workerActivity{workspace: []wsOp{{tool: "read_file", detail: `read_file(path="a.go")`}}},
		judgeKnownFailuresSection(map[string]criterionScore{"checks_pass": {Score: 0, Reason: "build failed"}}, 0.7))
	second := buildJudgePrompt(constitution, rubric, task, question, "a wholly different second answer", "diff two",
		workerActivity{workspace: []wsOp{{tool: "grep", detail: `grep(pattern="TODO")`}}}, "")

	const answerHeader = "\n\nAnswer to judge:\n"
	want := strings.Index(first, answerHeader) + len(answerHeader)
	if want <= len(answerHeader) {
		t.Fatalf("prompt missing %q header:\n%s", answerHeader, first)
	}

	got := 0
	for got < len(first) && got < len(second) && first[got] == second[got] {
		got++
	}
	if got < want {
		t.Fatalf("stable prefix ends at byte %d, want at least %d (through the answer header).\n"+
			"first differing byte is inside the supposedly round-invariant head - everything before the\n"+
			"answer must be byte-identical across rounds or the judge re-prefills the whole prompt.\n"+
			"round 1 from byte %d: %q\nround 2 from byte %d: %q",
			got, want, got, excerptAt(first, got), got, excerptAt(second, got))
	}
}

// excerptAt returns a short window of s starting at i, for prefix-mismatch reporting.
func excerptAt(s string, i int) string {
	if i >= len(s) {
		return ""
	}
	return s[i:min(i+80, len(s))]
}

// TestJudgeKnownFailuresSection_FormatsFailingCriteriaSorted checks the
// section names every below-threshold criterion, sorted, and skips a passing one.
func TestJudgeKnownFailuresSection_FormatsFailingCriteriaSorted(t *testing.T) {
	det := map[string]criterionScore{
		"mermaid_valid":     {Score: 0, Reason: "deterministic: invalid mermaid diagram at line 12: parse error"},
		"sufficient_length": {Score: 0, Reason: "deterministic: 0 chars"},
		"checks_pass":       {Score: 1, Reason: "deterministic: all checks passed"}, // passing - must not appear
	}
	got := judgeKnownFailuresSection(det, 0.7)
	if !strings.Contains(got, "mermaid_valid") || !strings.Contains(got, "invalid mermaid diagram") {
		t.Errorf("missing the mermaid_valid failure: %q", got)
	}
	if !strings.Contains(got, "sufficient_length") {
		t.Errorf("missing the sufficient_length failure: %q", got)
	}
	if strings.Contains(got, "checks_pass") {
		t.Errorf("a passing criterion must not appear in the known-failures section: %q", got)
	}
	if strings.Index(got, "mermaid_valid") > strings.Index(got, "sufficient_length") {
		t.Errorf("criteria should be listed alphabetically: %q", got)
	}
}

// TestBuildJudgePrompt_NoKnownFailuresOmitsSection checks the prompt is
// unchanged when nothing has failed deterministically.
func TestBuildJudgePrompt_NoKnownFailuresOmitsSection(t *testing.T) {
	det := map[string]criterionScore{"checks_pass": {Score: 1.0, Reason: "deterministic: all checks passed"}}
	known := judgeKnownFailuresSection(det, 0.7)
	if known != "" {
		t.Fatalf("judgeKnownFailuresSection = %q, want \"\" when every criterion passes", known)
	}

	q := questionContent("do the task")
	withoutArg := buildJudgePrompt("", "rubric text", "", q, "the answer", "", workerActivity{}, "")
	withEmptyKnown := buildJudgePrompt("", "rubric text", "", q, "the answer", "", workerActivity{}, known)
	if withEmptyKnown != withoutArg {
		t.Errorf("prompt changed even though nothing failed deterministically:\n--- want ---\n%s\n--- got ---\n%s", withoutArg, withEmptyKnown)
	}
}

// stuckJudge always reads a file and never calls submit_verdict - the judge
// RAN (it made tool calls, it spent turns) but never committed a verdict,
// the stand-in for exhausting gates.judge.max_iterations (#779). Distinct
// from flakyTransientJudge, which never runs at all.
type stuckJudge struct{ calls int32 }

func (j *stuckJudge) Name() string { return "stuck-judge" }

func (j *stuckJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&j.calls, 1)
		yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
	}
}

// TestRunJudgeAgent_ExhaustedIterationsReturnsErrJudgeNoVerdict is issue #779's
// test case 2: a judge that spends its whole iteration budget without ever
// calling submit_verdict must fail with the distinct ErrJudgeNoVerdict
// sentinel, not the same error shape a transport outage produces - node.go
// tells the two apart with errors.Is, never by matching this error's text.
func TestRunJudgeAgent_ExhaustedIterationsReturnsErrJudgeNoVerdict(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	judge := &stuckJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err == nil {
		t.Fatal("runJudgeAgent: expected an error - the judge never called submit_verdict")
	}
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Errorf("err = %v, want errors.Is(err, ErrJudgeNoVerdict) - the judge ran (it read files), it just never reached a verdict", err)
	}
	if isTransientJudgeErr(err) {
		t.Errorf("err = %v classified as transient/retryable, want the distinct no-verdict sentinel treated as permanent", err)
	}
	if atomic.LoadInt32(&judge.calls) < 2 {
		t.Errorf("judge model called %d times, want it to have actually run multiple turns before giving up", judge.calls)
	}
}

// changedFilesFixture builds a jail with n on-disk files under repo/, and a
// Config/workerActivity pointing the judge at all of them.
func changedFilesFixture(t *testing.T, n int) (Config, workerActivity) {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := jail.UserRoot("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	written := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("repo/file%02d.go", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("package repo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		written = append(written, name)
	}
	cfg := Config{Rubric: "score 0-10", Workspace: jail, WorkspaceUserID: "u1"}
	return cfg, workerActivity{written: written}
}

// TestRunJudgeAgent_ChangedFilesCoverage is issue #779's test cases 3 and 4:
// a changed-file set that fits inside maxChangedFiles produces a verdict with
// no truncation note, and one that exceeds it (18 files against the 12-file
// cap) carries the count the judge actually scored alongside the count that
// existed - not indistinguishable from a fully-scored verdict.
func TestRunJudgeAgent_ChangedFilesCoverage(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}

	t.Run("within caps: no truncation note", func(t *testing.T) {
		cfg, act := changedFilesFixture(t, 5)
		var prompt string
		factory := NewJudgeFactory(recordingJudge{prompt: &prompt}, nil, nil)
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", act, nil, func(*genai.Part) bool { return true })
		if err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		if v.ChangedFilesScored != 5 || v.ChangedFilesTotal != 5 {
			t.Errorf("verdict coverage = scored=%d total=%d, want 5/5", v.ChangedFilesScored, v.ChangedFilesTotal)
		}
		if strings.Contains(v.Feedback, "cap") {
			t.Errorf("feedback = %q, want no truncation note - every file fit", v.Feedback)
		}
	})

	t.Run("over the cap: verdict carries scored and total", func(t *testing.T) {
		cfg, act := changedFilesFixture(t, 18)
		var prompt string
		factory := NewJudgeFactory(recordingJudge{prompt: &prompt}, nil, nil)
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", act, nil, func(*genai.Part) bool { return true })
		if err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		if v.ChangedFilesScored != maxChangedFiles || v.ChangedFilesTotal != 18 {
			t.Errorf("verdict coverage = scored=%d total=%d, want %d/18", v.ChangedFilesScored, v.ChangedFilesTotal, maxChangedFiles)
		}
		if !strings.Contains(v.Feedback, fmt.Sprintf("%d", maxChangedFiles)) || !strings.Contains(v.Feedback, "18") {
			t.Errorf("feedback = %q, want it to name both the scored and total file counts", v.Feedback)
		}
	})
}

// TestRepeatsLastToolCall pins repeatsLastToolCall's contract directly (review
// on #857): only the two MOST RECENT calls matter, and both name and args must match.
func TestRepeatsLastToolCall(t *testing.T) {
	call := func(name string, args map[string]any) *genai.Content {
		return &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}}
	}
	tests := []struct {
		name  string
		calls []*genai.Content
		want  bool
	}{
		{"no calls", nil, false},
		{"one call", []*genai.Content{call("read_file", map[string]any{"path": "a"})}, false},
		{"identical consecutive", []*genai.Content{
			call("read_file", map[string]any{"path": "a"}),
			call("read_file", map[string]any{"path": "a"}),
		}, true},
		{"same name different args", []*genai.Content{
			call("read_file", map[string]any{"path": "a"}),
			call("read_file", map[string]any{"path": "b"}),
		}, false},
		{"different names", []*genai.Content{
			call("read_file", map[string]any{"path": "a"}),
			call("list_dir", map[string]any{"path": "a"}),
		}, false},
		{"repeat separated by a different call is not consecutive", []*genai.Content{
			call("read_file", map[string]any{"path": "a"}),
			call("list_dir", map[string]any{"path": "a"}),
			call("read_file", map[string]any{"path": "a"}),
		}, false},
		{"multi-key args match regardless of map order", []*genai.Content{
			call("edit", map[string]any{"path": "a", "text": "x"}),
			call("edit", map[string]any{"text": "x", "path": "a"}),
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repeatsLastToolCall(tc.calls); got != tc.want {
				t.Errorf("repeatsLastToolCall() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRepeatingTailSpan pins the pure detection function #889's runaway-loop
// guard is built on: a uniformly repeated unit at the end of a string is
// found and measured; a single occurrence, a too-short string, or ordinary
// non-repeating prose is not mistaken for one.
func TestRepeatingTailSpan(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := repeatingTailSpan("", judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("too short for any candidate unit", func(t *testing.T) {
		if got := repeatingTailSpan("abcdefg", judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("no repetition", func(t *testing.T) {
		s := "the quick brown fox jumps over a lazy dog while a cat watches quietly from the fence"
		if got := repeatingTailSpan(s, judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars); got != 0 {
			t.Errorf("got %d, want 0 (no unit actually repeats)", got)
		}
	})
	t.Run("uniform repeat spans the whole string", func(t *testing.T) {
		unit := "repeats! " // 9 chars, within [minUnit,maxUnit]
		s := strings.Repeat(unit, 30)
		if got := repeatingTailSpan(s, judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars); got != len(s) {
			t.Errorf("got %d, want %d (the entire string is one repeated unit)", got, len(s))
		}
	})
	t.Run("repeat only at the tail is found, prefix is ignored", func(t *testing.T) {
		unit := "loopy!!! " // 9 chars
		prefix := "an unrelated, non-repeating lead-in that never recurs "
		repeat := strings.Repeat(unit, 30)
		s := prefix + repeat
		if got := repeatingTailSpan(s, judgeRepeatMinUnitChars, judgeRepeatMaxUnitChars); got != len(repeat) {
			t.Errorf("got %d, want %d (only the tail repeat should count, not the prefix)", got, len(repeat))
		}
	})
}

// TestRepeatLoopDetector pins the stateful wrapper: it stays untripped under
// the trip threshold and trips once a genuine runaway repeat clears it,
// scanning only in judgeRepeatCheckStride-sized increments.
func TestRepeatLoopDetector(t *testing.T) {
	t.Run("varied text across many small appends never trips", func(t *testing.T) {
		var d repeatLoopDetector
		for i := 0; i < 50; i++ {
			d.observe(fmt.Sprintf("turn %d covers a distinct point that was not made before, ", i))
		}
		if d.tripped {
			t.Error("detector tripped on genuinely varied text")
		}
	})
	t.Run("a long uniform repeat trips", func(t *testing.T) {
		var d repeatLoopDetector
		unit := "This exact sentence repeats without variation. " // 49 chars
		for i := 0; i < 400 && !d.tripped; i++ {
			d.observe(unit)
		}
		if !d.tripped {
			t.Error("detector never tripped on a long uniform repeat")
		}
	})
}

// maxTokensRecordingJudge records the MaxOutputTokens the request actually
// carried (0 if req.Config was nil or the field unset) before submitting a
// normal verdict, proving the configured cap reaches the model call (#889).
type maxTokensRecordingJudge struct{ got *int32 }

func (maxTokensRecordingJudge) Name() string { return "max-tokens-recording-judge" }

func (j maxTokensRecordingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil {
			atomic.StoreInt32(j.got, req.Config.MaxOutputTokens)
		}
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil)
	}
}

// TestJudgeRequestCarriesConfiguredMaxOutputTokens proves cfg.JudgeMaxOutputTokens
// reaches the actual model request as genai.GenerateContentConfig.MaxOutputTokens.
func TestJudgeRequestCarriesConfiguredMaxOutputTokens(t *testing.T) {
	got := int32(-1)
	factory := NewJudgeFactory(maxTokensRecordingJudge{got: &got}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxOutputTokens: 4096}

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got != 4096 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 4096", got)
	}
}

// TestJudgeRequestZeroMaxOutputTokensLeavesUncapped proves the field's
// documented "<= 0 = uncapped" contract: an unset cfg.JudgeMaxOutputTokens
// must not set any MaxOutputTokens on the request at all.
func TestJudgeRequestZeroMaxOutputTokensLeavesUncapped(t *testing.T) {
	got := int32(-1)
	factory := NewJudgeFactory(maxTokensRecordingJudge{got: &got}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10"} // JudgeMaxOutputTokens left unset

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got != 0 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 0 (uncapped)", got)
	}
}

// garbledVerdictJudge always calls submit_verdict with an empty args map -
// exactly what a truncated/broken tool-call payload parses to upstream
// (openaimodel's parseJSONArgs swallows the JSON error and returns {}). The
// call is attempted every turn but its required "score" field is missing, so
// schema validation rejects it before the handler that populates the verdict
// ever runs.
type garbledVerdictJudge struct{ calls int32 }

func (j *garbledVerdictJudge) Name() string { return "garbled-verdict-judge" }

func (j *garbledVerdictJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&j.calls, 1)
		yield(stubCall(submitVerdictTool, map[string]any{}), nil)
	}
}

// TestRunJudgeAgent_GarbledSubmitVerdictRoutesToNoVerdict proves #889's fix: a
// submit_verdict call whose arguments fail schema validation (as a truncated
// tool-call payload would) must never be mistaken for a real submission - the
// round must end in ErrJudgeNoVerdict, never a "valid" zero-value verdict
// silently accepted as a scored pass or fail.
func TestRunJudgeAgent_GarbledSubmitVerdictRoutesToNoVerdict(t *testing.T) {
	judge := &garbledVerdictJudge{}
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict) - a garbled submit_verdict call must never look like a real verdict", err)
	}
}

// loopingJudgeModel emits reasoning text alongside a read_file call every
// turn - a plain text-only reply would otherwise end the agent run, so the
// tool call is what keeps the loop going - and that reasoning text is the
// EXACT same repeated phrase every time: the #889 incident's shape, a
// runaway generation loop that never reaches submit_verdict. The read_file
// path varies per call so this exercises only the #889 repeat guard, not the
// pre-existing #853 identical-tool-call stutter breaker.
type loopingJudgeModel struct{ calls int32 }

func (j *loopingJudgeModel) Name() string { return "looping-judge" }

func (j *loopingJudgeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := atomic.AddInt32(&j.calls, 1)
		resp := &model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "This exact sentence repeats without variation. ", Thought: true},
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": fmt.Sprintf("file%d.go", n)}}},
			}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}
		yield(resp, nil)
	}
}

// TestRunJudgeAgent_RunawayRepeatAbortsEarly proves #889's repeat guard: a
// judge stuck decoding the same text is cancelled and routed to the same
// no-verdict retry path a truncated reply takes - well before its iteration
// budget would otherwise let it keep running. The call count assertion is
// load-bearing: the round ends in ErrJudgeNoVerdict either way (the repeated
// text never parses as JSON regardless), so only a bounded call count proves
// the guard fired instead of the loop simply running to the turn cap.
func TestRunJudgeAgent_RunawayRepeatAbortsEarly(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	judge := &loopingJudgeModel{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	// A generous turn budget that would let the loop run hundreds of turns per
	// round if the repeat guard were not what stopped it.
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 1000}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	// #779's no-verdict path retries once with a fresh session, so up to two
	// rounds each trip the guard around its own ~160-180 call mark; 700 stays
	// far below the ~2000 calls the 1000-turn budget would allow unguarded.
	if got := atomic.LoadInt32(&judge.calls); got >= 700 {
		t.Errorf("judge model called %d times, want well under the 1000-turn budget - the repeat guard should have aborted early", got)
	}
}

// variedJudgeModel produces a few turns of genuinely different reasoning text
// - nowhere near the repeat guard's trip span - each paired with a read_file
// call to keep the loop going, before submitting a normal verdict.
type variedJudgeModel struct{ calls int32 }

func (j *variedJudgeModel) Name() string { return "varied-judge" }

func (j *variedJudgeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := atomic.AddInt32(&j.calls, 1)
		if n <= 3 {
			resp := &model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: fmt.Sprintf("Turn %d looks at a distinct part of the answer - finding %d is unrelated to the others.", n, n*13), Thought: true},
					{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": fmt.Sprintf("file%d.go", n)}}},
				}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}
			yield(resp, nil)
			return
		}
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// TestRunJudgeAgent_VariedReplyNotAborted is the repeat guard's other half:
// genuinely varied text spread across several turns must never trip it.
func TestRunJudgeAgent_VariedReplyNotAborted(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	judge := &variedJudgeModel{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Errorf("verdict score = %v, want 0.9 - varied filler text must not trip the repeat guard", v.Score)
	}
}

// stutterJudge repeats the exact same tool call twice (the model stutter
// #853 exists for), then - once forcedVerdictCallback has stripped its tools
// for repeating itself - closes with the verdict as plain-text JSON instead
// of a tool call.
type stutterJudge struct{ calls int32 }

func (j *stutterJudge) Name() string { return "stutter-judge" }

func (j *stutterJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := atomic.AddInt32(&j.calls, 1)
		if n <= 2 {
			yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
			return
		}
		if len(req.Tools) != 0 || (req.Config != nil && len(req.Config.Tools) != 0) {
			yield(nil, fmt.Errorf("expected no tools on the forced closing turn, got %d req.Tools", len(req.Tools)))
			return
		}
		yield(stubText(`{"score": 9, "criteria": {"accuracy": {"reason": "verified from prior reads", "score": 9}}, "feedback": ""}`), nil)
	}
}

// TestRunJudgeAgent_ForcedVerdictOnRepeatedToolCall is #853 test case (a): a
// judge that repeats an identical tool call gets its NEXT turn sent with no
// tools and the forced-close instruction, and the resulting plain-text
// verdict is parsed via the existing parseVerdict fallback.
func TestRunJudgeAgent_ForcedVerdictOnRepeatedToolCall(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	judge := &stutterJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6} // plenty of budget left - only the repeat should force the close

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Errorf("verdict score = %v, want 0.9 (parsed from the forced-close text)", v.Score)
	}
	if got := atomic.LoadInt32(&judge.calls); got != 3 {
		t.Errorf("judge model called %d times, want 3 (two identical read_file calls + the forced no-tools close)", got)
	}
}

// isFreshRound reports whether req is the first call of a brand-new round
// (no prior function call in its history yet) - each runJudgeRound call
// builds its own runner and session, so this is how a scripted fake model
// tells "still the same round" from "a fresh retry started".
func isFreshRound(req *model.LLMRequest) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil {
				return false
			}
		}
	}
	return true
}

// roundStuckThenRecoversJudge never reaches a verdict in its first round
// (repeats a tool call forever, like stuckJudge), then submits a normal
// verdict on the very first call of any later round - the stand-in for
// #853's "one retry with a fresh session" recovering a stuck round.
type roundStuckThenRecoversJudge struct {
	rounds int32
	calls  int32
}

func (j *roundStuckThenRecoversJudge) Name() string { return "round-stuck-then-recovers-judge" }

func (j *roundStuckThenRecoversJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&j.calls, 1)
		if isFreshRound(req) {
			atomic.AddInt32(&j.rounds, 1)
		}
		if atomic.LoadInt32(&j.rounds) == 1 {
			yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
			return
		}
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.85, "feedback": "recovered on retry"}), nil)
	}
}

// TestRunJudgeAgent_NoVerdictRetriesOnceThenSucceeds is #853 test case (b): a
// round that exhausts its budget without a verdict gets exactly one retry
// with a fresh session, and that retry's normal verdict is returned.
func TestRunJudgeAgent_NoVerdictRetriesOnceThenSucceeds(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	judge := &roundStuckThenRecoversJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.85 {
		t.Errorf("verdict score = %v, want 0.85 (the retry round that recovered)", v.Score)
	}
	if got := atomic.LoadInt32(&judge.rounds); got != 2 {
		t.Errorf("rounds started = %d, want 2 (the failed round + the one retry that recovered)", got)
	}
}

// alwaysStuckJudge never reaches a verdict, in any round - the stand-in for
// #853's "both attempts fail" case. roundsStarted counts independent rounds
// (fresh sessions), so the test asserts on rounds attempted, not the
// call-count mechanics of any one round.
type alwaysStuckJudge struct{ roundsStarted int32 }

func (j *alwaysStuckJudge) Name() string { return "always-stuck-judge" }

func (j *alwaysStuckJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if isFreshRound(req) {
			atomic.AddInt32(&j.roundsStarted, 1)
		}
		yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
	}
}

// TestRunJudgeAgent_NoVerdictRetryExhausted is #853 test case (c): when the
// retry ALSO ends without a verdict, runJudgeAgent returns the same
// ErrJudgeNoVerdict sentinel as before, having attempted exactly one retry.
func TestRunJudgeAgent_NoVerdictRetryExhausted(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	judge := &alwaysStuckJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err == nil {
		t.Fatal("runJudgeAgent: expected an error - the judge never reaches a verdict in either round")
	}
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Errorf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	if got := atomic.LoadInt32(&judge.roundsStarted); got != 2 {
		t.Errorf("rounds started = %d, want 2 (the original round + exactly one retry, no more)", got)
	}
}

// recordingToolsJudge captures whether req.Tools was populated when the
// judge model was called, so a test can prove forcedVerdictCallback left an
// ordinary round alone.
type recordingToolsJudge struct{ sawTools *bool }

func (recordingToolsJudge) Name() string { return "recording-tools-judge" }

func (j recordingToolsJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		*j.sawTools = len(req.Tools) > 0
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil)
	}
}

// TestRunJudgeAgent_NormalRoundKeepsTools is #853 test case (d): a judge that
// submits its verdict on the very first turn, well under budget, is left
// alone by forcedVerdictCallback - tools stay intact, one call, no retry.
func TestRunJudgeAgent_NormalRoundKeepsTools(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	var sawTools bool
	factory := NewJudgeFactory(recordingToolsJudge{sawTools: &sawTools}, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if !sawTools {
		t.Error("forcedVerdictCallback stripped tools on an ordinary first turn, well under budget")
	}
	if v.Score != 0.8 {
		t.Errorf("verdict score = %v, want 0.8", v.Score)
	}
}

// verdictToolCtx: minimal agent.Context for driving submit_verdict directly.
type verdictToolCtx struct {
	adkagent.ContextMock
	actions session.EventActions
}

func (c *verdictToolCtx) Actions() *session.EventActions { return &c.actions }
func (c *verdictToolCtx) Context() context.Context       { return context.Background() }

// runVerdictTool drives the tool's Run (not part of the public tool.Tool interface).
func runVerdictTool(t *testing.T, tl tool.Tool, args map[string]any) (map[string]any, error) {
	t.Helper()
	r, ok := tl.(interface {
		Run(adkagent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %T does not expose Run", tl)
	}
	return r.Run(&verdictToolCtx{}, args)
}

// TestSubmitVerdict_NearMissPayloads pins the two prod near-misses that made the
// judge "end without a verdict" (Langfuse trace 9ea8cbee38735f00ec66252922acf2aa):
// an anchor missing `kind`, and `shortfall`/`fix` sent as JSON null. Both must
// now validate and yield a usable verdict.
func TestSubmitVerdict_NearMissPayloads(t *testing.T) {
	t.Run("anchor missing kind", func(t *testing.T) {
		var sink verdict
		submit, err := newSubmitVerdictTool(&sink)
		if err != nil {
			t.Fatalf("newSubmitVerdictTool: %v", err)
		}
		args := map[string]any{
			"score": 4.0,
			"criteria": map[string]any{
				"cites_sources": map[string]any{
					"shortfall": "claim about foo.go is uncited",
					"fix":       "cite internal/foo.go",
					"score":     4.0,
					"anchor":    map[string]any{"text": "foo handles retries"},
				},
			},
			"feedback": "cite the repo claim",
		}
		if _, err := runVerdictTool(t, submit, args); err != nil {
			t.Fatalf("Run rejected payload with anchor missing kind: %v", err)
		}
		v := aggregateVerdict(sink)
		c, ok := v.Criteria["cites_sources"]
		if !ok {
			t.Fatal("criterion lost")
		}
		if c.Anchor == nil || c.Anchor.Kind != "quote" {
			t.Fatalf("anchor kind not inferred as quote: %+v", c.Anchor)
		}
		if c.Shortfall != "claim about foo.go is uncited" {
			t.Fatalf("shortfall = %q", c.Shortfall)
		}
	})

	t.Run("null shortfall and fix", func(t *testing.T) {
		var sink verdict
		submit, err := newSubmitVerdictTool(&sink)
		if err != nil {
			t.Fatalf("newSubmitVerdictTool: %v", err)
		}
		args := map[string]any{
			"score": 9.0,
			"criteria": map[string]any{
				"completeness": map[string]any{"shortfall": nil, "fix": nil, "score": 9.0},
			},
			"feedback": "",
		}
		if _, err := runVerdictTool(t, submit, args); err != nil {
			t.Fatalf("Run rejected payload with null shortfall: %v", err)
		}
		v := aggregateVerdict(sink)
		c, ok := v.Criteria["completeness"]
		if !ok {
			t.Fatal("criterion lost")
		}
		if c.Shortfall != "" || c.Score != 0.9 {
			t.Fatalf("got shortfall=%q score=%v, want empty/0.9", c.Shortfall, c.Score)
		}
	})

	// A genuinely malformed payload must still fail - and Run's error text is
	// what ADK hands back to the model as the tool result, so the in-round
	// retry sees WHY it was rejected.
	t.Run("still rejects wrong-typed criteria", func(t *testing.T) {
		var sink verdict
		submit, err := newSubmitVerdictTool(&sink)
		if err != nil {
			t.Fatalf("newSubmitVerdictTool: %v", err)
		}
		if _, err := runVerdictTool(t, submit, map[string]any{"score": 1.0, "criteria": []any{"nope"}}); err == nil {
			t.Fatal("want validation error for array criteria")
		}
	})
}

// TestInferAnchorKind pins the inference table, ambiguity included.
func TestInferAnchorKind(t *testing.T) {
	cases := []struct {
		in   anchorSpec
		want string
	}{
		{anchorSpec{Text: "q"}, "quote"},
		{anchorSpec{Path: "a/b.go"}, "path"},
		{anchorSpec{Expected: "a changelog"}, "omission"},
		{anchorSpec{Text: "q", Path: "a"}, ""},        // ambiguous: leave for sanitizeAnchors
		{anchorSpec{}, ""},                            // empty: nothing to infer
		{anchorSpec{Kind: "path", Text: "q"}, "path"}, // explicit kind wins
	}
	for _, tc := range cases {
		a := tc.in
		inferAnchorKind(&a)
		if a.Kind != tc.want {
			t.Errorf("inferAnchorKind(%+v) kind = %q, want %q", tc.in, a.Kind, tc.want)
		}
	}
}
