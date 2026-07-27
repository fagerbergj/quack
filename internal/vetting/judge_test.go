package vetting

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
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
				"I implemented game.go", workerActivity{}, func(*genai.Part) bool { return true })
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

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, hugeAnswer, workerActivity{}, func(*genai.Part) bool { return true })
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
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, func(*genai.Part) bool { return true })
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
	_, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, func(*genai.Part) bool { return true })
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
				"I implemented game.go", workerActivity{}, func(*genai.Part) bool { return true })
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
		"Paris.", workerActivity{}, func(*genai.Part) bool { return true })
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
