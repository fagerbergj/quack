package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/recordstore"
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

// newSpyReadTool is a read_file stand-in returning body and counting calls in calls.
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

// readFileResponseContent returns what a prior read_file call put in the request.
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

// scriptedJudge reads game.go, then scores from its body (pass iff it contains a test), proving the
// read loop grounds the score in the real source.
var scriptedJudge = fnLLM(func(req *model.LLMRequest) (*model.LLMResponse, error) {
	if content, seen := readFileResponseContent(req); seen {
		score := 0.2
		if strings.Contains(content, "func Test") {
			score = 0.9
		}
		return stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "graded from the file"}), nil
	}
	return stubCall("read_file", map[string]any{"path": "game.go"}), nil
})

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
			factory := NewJudgeFactory(scriptedJudge, []tool.Tool{readTool}, nil)
			q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
			v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
				"I implemented game.go", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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

// recordingJudge captures the assembled judge prompt into *prompt and submits a fixed verdict.
type recordingJudge struct{ prompt *string }

func (recordingJudge) Name() string { return "recording-judge" }

func (r recordingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		*r.prompt = stubAllText(req)
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil)
	}
}

// TestJudgeCharBudgetReservesConfiguredMaxOutputTokens: reserving a hardcoded 2000 instead of the configured
// max_output_tokens overflowed the slot and truncated the judge mid-thought.
func TestJudgeCharBudgetReservesConfiguredMaxOutputTokens(t *testing.T) {
	cfg := Config{JudgeContextWindow: 65_536, JudgeMaxOutputTokens: 8_192}
	got := judgeCharBudget(cfg)
	want := (65_536 - 8_192) * judgeCharsPerToken
	if got != want {
		t.Errorf("judgeCharBudget = %d, want %d (reserve must track cfg.JudgeMaxOutputTokens, not the %d-token fallback)",
			got, want, judgeOutputReserveTokens)
	}

	// Unset JudgeMaxOutputTokens still falls back to the fixed reserve.
	cfg2 := Config{JudgeContextWindow: 65_536}
	if got2 := judgeCharBudget(cfg2); got2 != (65_536-judgeOutputReserveTokens)*judgeCharsPerToken {
		t.Errorf("judgeCharBudget with unset max output tokens = %d, want fallback reserve applied", got2)
	}
}

// TestRunJudgeAgent_OverBudgetAnswerFitsBudget: an answer that would blow the judge's context window is
// clamped before the call, so the judge still returns a verdict instead of a 400.
func TestRunJudgeAgent_OverBudgetAnswerFitsBudget(t *testing.T) {
	var seenPrompt string
	factory := NewJudgeFactory(recordingJudge{prompt: &seenPrompt}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	// Far larger than any real judge slot (~125k tokens raw).
	hugeAnswer := strings.Repeat("the worker wrote a very long answer. ", 15_000)
	cfg := Config{Rubric: "score 0-10", JudgeContextWindow: 8_000} // small window forces a real clamp

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, hugeAnswer, workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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

// TestRunJudgeAgent_BuildsPromptOnceForARound: a round needing no clamp or retry builds the prompt once.
func TestRunJudgeAgent_BuildsPromptOnceForARound(t *testing.T) {
	factory := NewJudgeFactory(recordingJudge{prompt: new(string)}, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10"}

	before := judgePromptBuilds.Load()
	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "the answer", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.8 {
		t.Fatalf("verdict score = %v, want 0.8 (a clean, single-attempt round)", v.Score)
	}
	if got := judgePromptBuilds.Load() - before; got != 1 {
		t.Errorf("buildJudgePrompt calls = %d, want 1 (fitJudgeAnswer's prompt must be reused, not rebuilt)", got)
	}
}

// TestRunJudgeAgent_SessionIDIsChatIDNotConstant: runner.Run's session id becomes gen_ai.conversation.id,
// so a constant would merge every judge call into one Langfuse session.
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
	factory := NewJudgeFactory(scriptedJudge, []tool.Tool{spy}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
	cfg := Config{Rubric: "score 0-10", ChatID: "chat-42"}

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "I implemented game.go", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if gotSessionID != "chat-42" {
		t.Errorf("judge run session id = %q, want %q (the chat id, not the old \"verdict\" constant)", gotSessionID, "chat-42")
	}
}

// flakyTransientJudge fails its first `failures` calls with a 502 (a model swap in flight), then submits.
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

// TestRunJudgeAgent_RetriesTransientErrorThenSucceeds: a transient 502 is retried with backoff and yields a
// normal scored verdict, never a degrade.
func TestRunJudgeAgent_RetriesTransientErrorThenSucceeds(t *testing.T) {
	judge := &flakyTransientJudge{failures: 2}
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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

// TestRunJudgeAgent_PermanentTransientErrorFailsClosed: a judge that never recovers returns an error, not a pass.
func TestRunJudgeAgent_PermanentTransientErrorFailsClosed(t *testing.T) {
	judge := &flakyTransientJudge{failures: 100} // never recovers
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	_, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err == nil {
		t.Fatal("runJudgeAgent: expected an error when the judge model never recovers, got nil")
	}
	if got := atomic.LoadInt32(&judge.calls); got != judgeRetryAttempts {
		t.Errorf("judge model called %d times, want %d (judgeRetryAttempts; the short answer leaves the shrink-retry a no-op)", got, judgeRetryAttempts)
	}
}

// TestIsTransientJudgeErr: only endpoint faults are retried, never a rejection retrying would repeat.
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

// newSpyLoadSkillTool is a load_skill stand-in returning body and counting calls in calls.
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

// skillJudge loads a review skill, then scores from its body (pass iff it mandates a test), proving the
// judge grounds its score in a skill it loaded.
var skillJudge = fnLLM(func(req *model.LLMRequest) (*model.LLMResponse, error) {
	if instr, seen := skillResponseContent(req); seen {
		score := 0.3
		if strings.Contains(instr, "require a test") {
			score = 0.9
		}
		return stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "graded against the loaded skill"}), nil
	}
	return stubCall("load_skill", map[string]any{"name": "ponytail-review"}), nil
})

// TestJudgeLoadsSkillBeforeVerdict: the skill toolset reaches the judge and is callable before submit_verdict.
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
			factory := NewJudgeFactory(skillJudge, nil, []tool.Toolset{ts})
			q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
			v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
				"I implemented game.go", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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
var oneShotJudge = fnLLM(func(*model.LLMRequest) (*model.LLMResponse, error) {
	return stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil
})

// TestJudgeNoReadToolsOneShot: with no read tools (no workspace jail) the factory still builds a one-shot judge.
func TestJudgeNoReadToolsOneShot(t *testing.T) {
	factory := NewJudgeFactory(oneShotJudge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
		"Paris.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.8 {
		t.Errorf("verdict score = %v, want 0.8", v.Score)
	}
}

// toolCapturingJudge records the last request it saw (for inspecting its
// tool declarations) and always passes.
func toolCapturingJudge(got **model.LLMRequest) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		*got = req
		return stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": ""}), nil
	}
}

// TestJudgeFactoryIncludesArtifactTools: cfg.JudgeArtifactTools reach the round's tool declarations.
func TestJudgeFactoryIncludesArtifactTools(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	artTools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatalf("NewJudgeArtifactTools: %v", err)
	}
	var req *model.LLMRequest
	factory := NewJudgeFactory(toolCapturingJudge(&req), nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X"}}}
	cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: artTools}
	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "answer", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if req == nil {
		t.Fatal("judge model never received a request")
	}
	if !stubHasTool(req, "list_artifacts") || !stubHasTool(req, "read_artifact") {
		t.Errorf("judge round tools missing list_artifacts/read_artifact")
	}
}

// artifactDiscardJudge always passes without reading, via the text-JSON fallback (submit_verdict's schema
// has no "passed"); its 2nd request is captured to check what the re-judge named.
func artifactDiscardJudge(calls *int32, second *string) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if atomic.AddInt32(calls, 1) == 2 && second != nil {
			*second = stubAllText(req)
		}
		return stubText(`{"score": 0.9, "passed": true, "feedback": ""}`), nil
	}
}

// spyReadArtifactArgs mirrors read_artifact's id-addressed input.
type spyReadArtifactArgs struct {
	ID string `json:"id"`
}

// newSpyReadArtifactTool returns a stand-in read_artifact tool that bumps
// calls each time the judge invokes it.
func newSpyReadArtifactTool(t *testing.T, calls *int32) tool.Tool {
	t.Helper()
	rt, err := functiontool.New[spyReadArtifactArgs, string](
		functiontool.Config{Name: "read_artifact", Description: "Read an artifact by id."},
		func(_ adkagent.Context, _ spyReadArtifactArgs) (string, error) {
			atomic.AddInt32(calls, 1)
			return "artifact content", nil
		},
	)
	if err != nil {
		t.Fatalf("spy read_artifact tool: %v", err)
	}
	return rt
}

// sawReadArtifactResponse reports whether req already carries a completed
// read_artifact call/response pair.
func sawReadArtifactResponse(req *model.LLMRequest) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "read_artifact" {
				return true
			}
		}
	}
	return false
}

// artifactReadingJudge calls read_artifact once, then passes - proving a
// round that DID read is never discarded.
func artifactReadingJudge(id string) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if sawReadArtifactResponse(req) {
			return stubText(`{"score": 0.9, "passed": true, "feedback": ""}`), nil
		}
		return stubCall("read_artifact", map[string]any{"id": id}), nil
	}
}

// TestJudgeRepoUnreadPassDiscardRound drives unreadPass through a real round via the text-JSON fallback,
// so v.Passed is actually true.
func TestJudgeRepoUnreadPassDiscardRound(t *testing.T) {
	var calls int32
	var second string
	readTool := newSpyReadTool(t, "package main", new(int32)) // present but never called
	factory := NewJudgeFactory(artifactDiscardJudge(&calls, &second), []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}
	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q, "I implemented game.go",
		workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one discard, one re-judge)", calls)
	}
	if !v.Passed {
		t.Errorf("final verdict Passed = false, want true (accepted on second offence)")
	}
}

// TestJudgeArtifactReadDiscard: a PASS that never read an artifact the worker wrote is re-judged once,
// naming the ids; reading first, or writing nothing, leaves the verdict alone.
func TestJudgeArtifactReadDiscard(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X"}}}
	wroteArtifact := workerActivity{artifactsWritten: []string{"doc:abc123"}}

	t.Run("pass with zero artifact reads is re-judged, naming the id", func(t *testing.T) {
		var calls int32
		var second string
		readTool := newSpyReadArtifactTool(t, new(int32)) // present but never called
		factory := NewJudgeFactory(artifactDiscardJudge(&calls, &second), nil, nil)
		cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: []tool.Tool{readTool}}
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "read_artifact to see the research",
			wroteArtifact, nil, nil, func(*genai.Part) bool { return true })
		if err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		if calls != 2 {
			t.Fatalf("model calls = %d, want 2 (one discard, one re-judge)", calls)
		}
		if !strings.Contains(second, "doc:abc123") {
			t.Errorf("re-judge prompt = %q, want it to name doc:abc123", second)
		}
		if !v.Passed {
			t.Errorf("final verdict Passed = false, want true (accepted on second offence)")
		}
	})

	t.Run("pass after reading is accepted, no re-judge", func(t *testing.T) {
		var reads int32
		readTool := newSpyReadArtifactTool(t, &reads)
		factory := NewJudgeFactory(artifactReadingJudge("doc:abc123"), nil, nil)
		cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: []tool.Tool{readTool}}
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "the research is in the artifact",
			wroteArtifact, nil, nil, func(*genai.Part) bool { return true })
		if err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		// A re-judge would have read again (artifactReadingJudge always reads
		// before passing), so exactly one call proves no re-judge fired.
		if reads != 1 {
			t.Errorf("read_artifact calls = %d, want 1 (no re-judge)", reads)
		}
		if !v.Passed {
			t.Errorf("verdict Passed = false, want true")
		}
	})

	t.Run("a rendered surface alone still requires a read", func(t *testing.T) {
		var calls int32
		var second string
		factory := NewJudgeFactory(artifactDiscardJudge(&calls, &second), nil, nil)
		cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: []tool.Tool{newSpyReadArtifactTool(t, new(int32))}}
		rendered := workerActivity{rendered: []string{"a2ui_surface:acme-widgets-pr-1-tutor", "quiz_key:acme-widgets-pr-1-tutor"}}
		if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "Rendered the walkthrough.", rendered, nil, nil, func(*genai.Part) bool { return true }); err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		if calls != 2 || !strings.Contains(second, "quiz_key:acme-widgets-pr-1-tutor") {
			t.Errorf("calls = %d, re-judge prompt = %q; want a re-judge naming the quiz key", calls, second)
		}
	})

	t.Run("worker wrote nothing: no discard even with zero reads", func(t *testing.T) {
		var calls int32
		var second string
		factory := NewJudgeFactory(artifactDiscardJudge(&calls, &second), nil, nil)
		v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, q,
			"a plain answer", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
		if err != nil {
			t.Fatalf("runJudgeAgent: %v", err)
		}
		if calls != 1 {
			t.Errorf("model calls = %d, want 1 (no re-judge)", calls)
		}
		if !v.Passed {
			t.Errorf("verdict Passed = false, want true")
		}
	})
}

// TestJudgeBehaviourSelectsClause: the read-tools clause appears only with read tools, no-tools only without.
func TestJudgeBehaviourSelectsClause(t *testing.T) {
	with := mustJudgeBehaviour(t, true, false)
	if !strings.Contains(with, "read-only workspace tools") || strings.Contains(with, "You have no workspace tools") {
		t.Errorf("read-tools behaviour missing its clause: %q", with)
	}
	// Without the clone-root grounding a judge retried "/frontend" until the repeat guard gave up.
	if !strings.Contains(with, "plain repo-relative paths") || !strings.Contains(with, "NEVER use a leading slash") {
		t.Errorf("read-tools behaviour missing repo-relative path grounding: %q", with)
	}
	without := mustJudgeBehaviour(t, false, false)
	if !strings.Contains(without, "You have no workspace tools") || strings.Contains(without, "read-only workspace tools") {
		t.Errorf("no-tools behaviour missing its clause: %q", without)
	}
	// artifact_tools is unconditional, present with or without repo read tools.
	if !strings.Contains(with, "list_artifacts") || !strings.Contains(without, "list_artifacts") {
		t.Errorf("artifact_tools clause missing: with=%q without=%q", with, without)
	}
	// The skills clause appears only when the judge holds the skill toolset.
	withSkills := mustJudgeBehaviour(t, false, true)
	if !strings.Contains(withSkills, "skill tools") || !strings.Contains(withSkills, "load a relevant") {
		t.Errorf("with-skills behaviour missing its clause: %q", withSkills)
	}
	if strings.Contains(without, "skill tools") {
		t.Errorf("no-skills behaviour must not mention skill tools: %q", without)
	}
}

// TestJudgePromptScopedToNodeNotOrchestratorFileCount: the judge sees the node's own task and the real
// clone diff, never an orchestrator-style <changed_files count=...> it was not given.
func TestJudgePromptScopedToNodeNotOrchestratorFileCount(t *testing.T) {
	nodeTask := "<permissions>push_commits_to_pr</permissions>\n<deliverable>a commit</deliverable>\n" +
		"<issue number=\"7\"><title>t</title><description>d</description></issue>\n\n" +
		"YOUR TASK - do this, and ONLY this:\nFix the failing build check in internal/foo.go."
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "irrelevant outer question"}}}
	changedFiles := "diff --git a/internal/foo.go b/internal/foo.go\n--- a/internal/foo.go\n+++ b/internal/foo.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"

	prompt := buildJudgePrompt("", "rubric text", nodeTask, "", question, "answer text", changedFiles, workerActivity{}, "")

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

// TestBuildJudgePromptSectionOrder: round-invariant sections lead and the judged answer trails, so the
// prefix stays a prompt-cache hit across rounds.
func TestBuildJudgePromptSectionOrder(t *testing.T) {
	act := workerActivity{workspace: []wsOp{{tool: "read_file", detail: `read_file(path="README.md")`}}}
	det := map[string]criterionScore{"checks_pass": {Score: 0, Reason: "deterministic: build failed"}}
	known := judgeKnownFailuresSection(det, 0.7)

	prompt := buildJudgePrompt("the constitution", "the rubric", "the node task", "",
		questionContent("the question"), "the answer being scored", "the changed files diff", act, known)

	sections := []string{"the constitution", "the rubric", "the node task", "the question",
		"Workspace activity", "the changed files diff", judgeKnownFailuresHeader, "the answer being scored"}
	last := -1
	for _, s := range sections {
		idx := strings.Index(prompt, s)
		if idx < 0 {
			t.Fatalf("prompt missing section %q:\n%s", s, prompt)
		}
		if idx < last {
			t.Fatalf("section %q is out of order (want constitution, rubric, task, question, ledger, changed files, known failures, answer):\n%s", s, prompt)
		}
		last = idx
	}
}

// TestBuildJudgePromptStablePrefixIsByteIdentical: two rounds differing only in the answer must match
// byte for byte through "Answer to judge:", since a leaked clock or run id moves the cache break earlier.
func TestBuildJudgePromptStablePrefixIsByteIdentical(t *testing.T) {
	const constitution, rubric, task = "the constitution", "the rubric", "the node task"
	question := questionContent("the question")
	act := workerActivity{workspace: []wsOp{{tool: "read_file", detail: `read_file(path="a.go")`}}}
	known := judgeKnownFailuresSection(map[string]criterionScore{"checks_pass": {Score: 0, Reason: "build failed"}}, 0.7)

	first := buildJudgePrompt(constitution, rubric, task, "", question, "the first answer", "diff one", act, known)
	second := buildJudgePrompt(constitution, rubric, task, "", question, "a wholly different second answer", "diff one", act, known)

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
	withoutArg := buildJudgePrompt("", "rubric text", "", "", q, "the answer", "", workerActivity{}, "")
	withEmptyKnown := buildJudgePrompt("", "rubric text", "", "", q, "the answer", "", workerActivity{}, known)
	if withEmptyKnown != withoutArg {
		t.Errorf("prompt changed even though nothing failed deterministically:\n--- want ---\n%s\n--- got ---\n%s", withoutArg, withEmptyKnown)
	}
}

// stuckJudge always reads a file and never submits: it ran but never committed a verdict, unlike
// flakyTransientJudge, which never runs at all.
type stuckJudge struct{ calls int32 }

func (j *stuckJudge) Name() string { return "stuck-judge" }

func (j *stuckJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&j.calls, 1)
		yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
	}
}

// TestRunJudgeAgent_ExhaustedIterationsReturnsErrJudgeNoVerdict: exhausting the iteration budget yields
// ErrJudgeNoVerdict, which callers tell apart from a transport outage with errors.Is.
func TestRunJudgeAgent_ExhaustedIterationsReturnsErrJudgeNoVerdict(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	judge := &stuckJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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

// TestRunJudgeAgent_ChangedFilesCoverage: files within maxChangedFiles carry no truncation note; 18 files
// against the 12-file cap report scored versus total.
func TestRunJudgeAgent_ChangedFilesCoverage(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}

	t.Run("within caps: no truncation note", func(t *testing.T) {
		cfg, act := changedFilesFixture(t, 5)
		var prompt string
		factory := NewJudgeFactory(recordingJudge{prompt: &prompt}, nil, nil)
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", act, nil, nil, func(*genai.Part) bool { return true })
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
		v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", act, nil, nil, func(*genai.Part) bool { return true })
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

// TestRepeatsLastToolCall: only the two most recent calls matter, and both name and args must match.
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

// TestRepeatingTailSpan: a repeated unit at the end is found and measured; a single occurrence, a short
// string, or ordinary prose is not.
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

// TestRepeatLoopDetector: untripped under the threshold, tripped past it, scanning in stride increments.
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

// maxTokensRecordingJudge records the request's MaxOutputTokens (0 if unset) before submitting.
func maxTokensRecordingJudge(got *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if req.Config != nil {
			atomic.StoreInt32(got, req.Config.MaxOutputTokens)
		}
		return stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil
	}
}

// TestJudgeRequestCarriesConfiguredMaxOutputTokens proves cfg.JudgeMaxOutputTokens
// reaches the actual model request as genai.GenerateContentConfig.MaxOutputTokens.
func TestJudgeRequestCarriesConfiguredMaxOutputTokens(t *testing.T) {
	got := int32(-1)
	factory := NewJudgeFactory(maxTokensRecordingJudge(&got), nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxOutputTokens: 4096}

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got != 4096 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 4096", got)
	}
}

// TestJudgeRequestZeroMaxOutputTokensLeavesUncapped: an unset cap sets no MaxOutputTokens at all.
func TestJudgeRequestZeroMaxOutputTokensLeavesUncapped(t *testing.T) {
	got := int32(-1)
	factory := NewJudgeFactory(maxTokensRecordingJudge(&got), nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10"} // JudgeMaxOutputTokens left unset

	if _, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got != 0 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 0 (uncapped)", got)
	}
}

// garbledVerdictJudge calls submit_verdict with {} every turn, which is what openaimodel's parseJSONArgs
// makes of a truncated payload; schema validation rejects it before the handler runs.
func garbledVerdictJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		atomic.AddInt32(calls, 1)
		return stubCall(submitVerdictTool, map[string]any{}), nil
	}
}

// TestRunJudgeAgent_GarbledSubmitVerdictRoutesToNoVerdict: a schema-invalid submit_verdict ends in
// ErrJudgeNoVerdict, never a zero-value verdict accepted as scored.
func TestRunJudgeAgent_GarbledSubmitVerdictRoutesToNoVerdict(t *testing.T) {
	var calls int32
	judge := garbledVerdictJudge(&calls)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict) - a garbled submit_verdict call must never look like a real verdict", err)
	}
}

// loopingJudgeModel repeats the exact same reasoning text every turn beside a read_file call (varying path,
// so only the repeat guard fires, not the identical-call stutter breaker).
func loopingJudgeModel(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		resp := &model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "This exact sentence repeats without variation. ", Thought: true},
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": fmt.Sprintf("file%d.go", n)}}},
			}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}
		return resp, nil
	}
}

// TestRunJudgeAgent_RunawayRepeatAbortsEarly: a judge decoding the same text is cancelled well before its
// turn budget; the call-count bound is what proves the guard fired.
func TestRunJudgeAgent_RunawayRepeatAbortsEarly(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	var calls int32
	judge := loopingJudgeModel(&calls)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	// A generous turn budget that would let the loop run hundreds of turns per
	// round if the repeat guard were not what stopped it.
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 1000}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	// The no-verdict retry means two rounds each trip near call 160-180; 700 is far below the
	// ~2000 calls the 1000-turn budget allows unguarded.
	if got := atomic.LoadInt32(&calls); got >= 700 {
		t.Errorf("judge model called %d times, want well under the 1000-turn budget - the repeat guard should have aborted early", got)
	}
}

// variedJudgeModel gives a few turns of distinct reasoning (well under the repeat guard) with read_file
// calls, then submits.
func variedJudgeModel(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		if n <= 3 {
			resp := &model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: fmt.Sprintf("Turn %d looks at a distinct part of the answer - finding %d is unrelated to the others.", n, n*13), Thought: true},
					{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": fmt.Sprintf("file%d.go", n)}}},
				}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}
			return resp, nil
		}
		return stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": ""}), nil
	}
}

// TestRunJudgeAgent_VariedReplyNotAborted is the repeat guard's other half:
// genuinely varied text spread across several turns must never trip it.
func TestRunJudgeAgent_VariedReplyNotAborted(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	var calls int32
	judge := variedJudgeModel(&calls)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Errorf("verdict score = %v, want 0.9 - varied filler text must not trip the repeat guard", v.Score)
	}
}

// stutterJudge repeats one tool call twice, then answers the forced close with plain-text JSON.
func stutterJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		if n <= 2 {
			return stubCall("read_file", map[string]any{"path": "game.go"}), nil
		}
		if err := strippedCloseErr(req); err != nil {
			return nil, err
		}
		return stubText(`{"score": 3, "criteria": {"accuracy": {"reason": "verified from prior reads", "score": 3}}, "feedback": ""}`), nil
	}
}

// forcedCloseErr: a forced-close turn keeps every tool declared (the prompt head, and so the
// server's prefix cache, is unchanged) and disables calling them with tool_choice none.
func forcedCloseErr(req *model.LLMRequest) error {
	if req.Config == nil || len(req.Config.Tools) == 0 {
		return errors.New("forced-close turn dropped the tool declarations")
	}
	if tc := req.Config.ToolConfig; tc == nil || tc.FunctionCallingConfig == nil || tc.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone {
		return fmt.Errorf("forced-close turn tool config = %+v, want mode NONE", tc)
	}
	return nil
}

// strippedCloseErr: a stutter's close (and the fallback after an empty tool_choice-none
// close) carries no tools at all, so a parser has nothing to drop.
func strippedCloseErr(req *model.LLMRequest) error {
	if len(req.Tools) != 0 || (req.Config != nil && len(req.Config.Tools) != 0) {
		return fmt.Errorf("expected no tools on a stripped close, got %d req.Tools", len(req.Tools))
	}
	return nil
}

// TestRunJudgeAgent_ForcedVerdictOnRepeatedToolCall: a repeated identical call gets its next turn with no
// tools and the force-close instruction; the text verdict is parsed by the fallback.
func TestRunJudgeAgent_ForcedVerdictOnRepeatedToolCall(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	var calls int32
	judge := stutterJudge(&calls)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6} // plenty of budget left - only the repeat should force the close

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 1.0 {
		t.Errorf("verdict score = %v, want 1.0 (parsed from the forced-close text)", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("judge model called %d times, want 3 (two identical read_file calls + the forced no-tools close)", got)
	}
}

// isFreshRound: req has no prior function call, i.e. the first call of a new round (each runJudgeRound
// builds its own session).
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

// roundStuckThenRecoversJudge repeats a tool call forever in its first round, then submits on the first
// call of any later round.
func roundStuckThenRecoversJudge(rounds, calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		atomic.AddInt32(calls, 1)
		if isFreshRound(req) {
			atomic.AddInt32(rounds, 1)
		}
		if atomic.LoadInt32(rounds) == 1 {
			return stubCall("read_file", map[string]any{"path": "game.go"}), nil
		}
		return stubCall(submitVerdictTool, map[string]any{"score": 0.85, "feedback": "recovered on retry"}), nil
	}
}

// TestRunJudgeAgent_NoVerdictRetriesOnceThenSucceeds: a verdict-less round gets one fresh-session retry.
func TestRunJudgeAgent_NoVerdictRetriesOnceThenSucceeds(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	var rounds, calls int32
	judge := roundStuckThenRecoversJudge(&rounds, &calls)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.85 {
		t.Errorf("verdict score = %v, want 0.85 (the retry round that recovered)", v.Score)
	}
	if got := atomic.LoadInt32(&rounds); got != 2 {
		t.Errorf("rounds started = %d, want 2 (the failed round + the one retry that recovered)", got)
	}
}

// alwaysStuckJudge never reaches a verdict; roundsStarted counts fresh sessions.
func alwaysStuckJudge(roundsStarted *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if isFreshRound(req) {
			atomic.AddInt32(roundsStarted, 1)
		}
		return stubCall("read_file", map[string]any{"path": "game.go"}), nil
	}
}

// TestRunJudgeAgent_NoVerdictRetryExhausted: a failed retry returns ErrJudgeNoVerdict after exactly one retry.
func TestRunJudgeAgent_NoVerdictRetryExhausted(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	var roundsStarted int32
	judge := alwaysStuckJudge(&roundsStarted)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 2}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err == nil {
		t.Fatal("runJudgeAgent: expected an error - the judge never reaches a verdict in either round")
	}
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Errorf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	if got := atomic.LoadInt32(&roundsStarted); got != 2 {
		t.Errorf("rounds started = %d, want 2 (the original round + exactly one retry, no more)", got)
	}
}

// recordingToolsJudge records whether req.Tools was populated.
func recordingToolsJudge(sawTools *bool) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		*sawTools = len(req.Tools) > 0
		return stubCall(submitVerdictTool, map[string]any{"score": 0.8, "feedback": ""}), nil
	}
}

// TestRunJudgeAgent_NormalRoundKeepsTools: a first-turn verdict keeps its tools, one call, no retry.
func TestRunJudgeAgent_NormalRoundKeepsTools(t *testing.T) {
	var reads int32
	readTool := newSpyReadTool(t, "package x\n", &reads)
	var sawTools bool
	factory := NewJudgeFactory(recordingToolsJudge(&sawTools), []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
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

// TestSubmitVerdict_NearMissPayloads: an anchor missing `kind` and null `shortfall`/`fix` both validate.
func TestSubmitVerdict_NearMissPayloads(t *testing.T) {
	t.Run("anchor missing kind", func(t *testing.T) {
		var sink verdict
		submit, err := newSubmitVerdictTool(&sink, nil)
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
		submit, err := newSubmitVerdictTool(&sink, nil)
		if err != nil {
			t.Fatalf("newSubmitVerdictTool: %v", err)
		}
		args := map[string]any{
			"score": 3.0,
			"criteria": map[string]any{
				"completeness": map[string]any{"shortfall": nil, "fix": nil, "score": 3.0},
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
		if c.Shortfall != "" || c.Score != 1.0 {
			t.Fatalf("got shortfall=%q score=%v, want empty/1.0", c.Shortfall, c.Score)
		}
	})

	// Run's error text is the tool result ADK returns, so the in-round retry sees why it was rejected.
	t.Run("still rejects wrong-typed criteria", func(t *testing.T) {
		var sink verdict
		submit, err := newSubmitVerdictTool(&sink, nil)
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

// TestCommitHygieneEvidenceSection: a file the task never names is flagged; named files and no files are not.
func TestCommitHygieneEvidenceSection(t *testing.T) {
	act := workerActivity{written: []string{"internal/foo/bar.go", "internal/foo/baz_test.go"}}
	got := commitHygieneEvidenceSection("Fix the off-by-one bug in bar.go", act)
	if !strings.Contains(got, "internal/foo/baz_test.go") {
		t.Errorf("evidence missing unnamed file: %q", got)
	}
	if strings.Contains(got, "internal/foo/bar.go") {
		t.Errorf("evidence wrongly flagged a task-named file: %q", got)
	}

	if got := commitHygieneEvidenceSection("t", workerActivity{}); got != "" {
		t.Errorf("no written files should yield no section, got %q", got)
	}
	if got := commitHygieneEvidenceSection("touch a.go and b.go", workerActivity{written: []string{"a.go", "b.go"}}); got != "" {
		t.Errorf("all-named files should yield no section, got %q", got)
	}
}

// garbledThenSubmitsJudge answers unparseable text first, then calls submit_verdict once nudged.
func garbledThenSubmitsJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		if n == 1 {
			return stubText(`{"score": 3, "criteria": {"constructive_actionable": "reason": "garbled"}}`), nil
		}
		return stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": "submitted on the nudge"}), nil
	}
}

// TestRunJudgeAgent_SubmitNudgeRecoversGarbledText: an unparseable text turn gets one in-session nudge, and
// submitting on it avoids a fresh round.
func TestRunJudgeAgent_SubmitNudgeRecoversGarbledText(t *testing.T) {
	var calls int32
	judge := garbledThenSubmitsJudge(&calls)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Errorf("verdict score = %v, want 0.9 (recovered via the in-session nudge)", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("judge model called %d times, want 2 (garbled text + the nudge) - no fresh-session retry should have run", got)
	}
}

// neverSubmitsTextOnlyJudge always answers the same unparseable text; the nudge must not invent a verdict.
func neverSubmitsTextOnlyJudge(roundsStarted *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		// No FunctionCall ever enters this session, so only a single-message request marks a new session.
		if len(req.Contents) == 1 {
			atomic.AddInt32(roundsStarted, 1)
		}
		return stubText(`{"score": "not-parseable-`), nil
	}
}

// TestRunJudgeAgent_SubmitNudgeExhaustedStillNoVerdict: the nudge and one fresh retry, then ErrJudgeNoVerdict.
func TestRunJudgeAgent_SubmitNudgeExhaustedStillNoVerdict(t *testing.T) {
	var roundsStarted int32
	judge := neverSubmitsTextOnlyJudge(&roundsStarted)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 6}

	_, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	if got := atomic.LoadInt32(&roundsStarted); got != 2 {
		t.Errorf("rounds started = %d, want 2 (the original round, nudged in-session, + exactly one fresh-session retry)", got)
	}
}

// forceClosedGarbledJudge makes two read_file calls (maxIters=3), so its third call is the forced close,
// which it answers with unparseable text.
func forceClosedGarbledJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		if n < 3 {
			return stubCall("read_file", map[string]any{"path": fmt.Sprintf("file%d.go", n)}), nil
		}
		check := forcedCloseErr // the first close keeps tools; its unparseable result earns one stripped close
		if n > 3 {
			check = strippedCloseErr
		}
		if err := check(req); err != nil {
			return nil, err
		}
		return stubText(`{"score": "not-parseable-`), nil
	}
}

// noneCloseJudge reads twice, then answers its last-turn tool_choice-none close with reply
// (a verdict, or nothing as vLLM gives for a dropped call); a stripped close always gets a verdict.
type noneCloseJudge struct {
	calls     int32
	reply     string
	maxTokens bool // the none close is cut at the token cap
}

func (*noneCloseJudge) Name() string { return "none-close-judge" }

func (j *noneCloseJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := atomic.AddInt32(&j.calls, 1)
		switch {
		case n < 3:
			yield(stubCall("read_file", map[string]any{"path": fmt.Sprintf("f%d.go", n)}), nil)
		case strippedCloseErr(req) == nil:
			yield(stubText(`{"score": 3, "criteria": {"accuracy": {"reason": "ok", "score": 3}}, "feedback": ""}`), nil)
		case forcedCloseErr(req) != nil:
			yield(nil, forcedCloseErr(req))
		case j.maxTokens:
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: `{"score": 3, "crit`}}}, FinishReason: genai.FinishReasonMaxTokens, TurnComplete: true}, nil)
		default:
			yield(stubText(j.reply), nil)
		}
	}
}

// nudgeCloseJudge answers its first turn with prose, so the submit nudge lands on the last
// allowed turn: that close is tool_choice none, comes back empty, and only a stripped close answers.
type nudgeCloseJudge struct{ calls int32 }

func (*nudgeCloseJudge) Name() string { return "nudge-close-judge" }

func (j *nudgeCloseJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case atomic.AddInt32(&j.calls, 1) == 1:
			yield(stubText("Let me think about the answer."), nil)
		case strippedCloseErr(req) == nil:
			yield(stubText(`{"score": 3, "criteria": {"accuracy": {"reason": "ok", "score": 3}}, "feedback": ""}`), nil)
		default:
			yield(stubText(""), nil)
		}
	}
}

// TestForcedClose_NudgeTakesStrippedFallback: a submit nudge that is itself the forced close gets
// the same stripped fallback; a close cut at the token cap gets none (stripping would not shorten it).
func TestForcedClose_NudgeTakesStrippedFallback(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	nudged := &nudgeCloseJudge{}
	v, _, err := runJudgeRound(t.Context(), NewJudgeFactory(nudged, []tool.Tool{newSpyReadTool(t, "x", new(int32))}, nil), Config{Rubric: "score 0-10", JudgeMaxIterations: 2}, q, "done.", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if err != nil || v.Score != 1.0 || atomic.LoadInt32(&nudged.calls) != 3 {
		t.Errorf("nudge close: score %v err %v after %d calls, want a verdict after 3", v.Score, err, nudged.calls)
	}
	capped := &noneCloseJudge{maxTokens: true}
	_, _, err = runJudgeRound(t.Context(), NewJudgeFactory(capped, []tool.Tool{newSpyReadTool(t, "x", new(int32))}, nil), Config{Rubric: "score 0-10", JudgeMaxIterations: 3}, q, "done.", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) || atomic.LoadInt32(&capped.calls) != 3 {
		t.Errorf("token-capped close: err %v after %d calls, want no verdict after 3", err, capped.calls)
	}
}

// TestForcedClose_NoneThenStripped: a tool_choice-none close that parses is the verdict (cache
// kept, no extra call); one that comes back empty gets exactly one more close with tools stripped.
func TestForcedClose_NoneThenStripped(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 3}
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	for _, tc := range []struct {
		reply string
		calls int32
	}{
		{`{"score": 3, "criteria": {"accuracy": {"reason": "ok", "score": 3}}, "feedback": ""}`, 3},
		{"", 4},
	} {
		judge := &noneCloseJudge{reply: tc.reply}
		v, _, err := runJudgeRound(t.Context(), NewJudgeFactory(judge, []tool.Tool{readTool}, nil), cfg, q, "done.", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true })
		if err != nil || v.Score != 1.0 || atomic.LoadInt32(&judge.calls) != tc.calls {
			t.Errorf("reply %q: score %v err %v after %d calls, want a verdict after %d", tc.reply, v.Score, err, judge.calls, tc.calls)
		}
	}
}

// TestRunJudgeAgent_ForcedCloseSkipsSubmitNudge: a forced close has no tools, so no submit nudge follows.
// Calls runJudgeRound directly to isolate it from the fresh-session retry.
func TestRunJudgeAgent_ForcedCloseSkipsSubmitNudge(t *testing.T) {
	var calls int32
	judge := forceClosedGarbledJudge(&calls)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 3}

	_, _, err := runJudgeRound(t.Context(), factory, cfg, q, "done.", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("judge model called %d times, want exactly 4 (2 reads + the tool_choice-none close + one stripped close) - no nudge call should follow a forced close", got)
	}
}

// repeatTrippedJudgeModel repeats plain text beside a varying tool call, tripping the repeat guard;
// nudgeCalls counts requests carrying judgeSubmitNudge.
func repeatTrippedJudgeModel(calls, nudgeCalls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && strings.Contains(p.Text, judgeSubmitNudge) {
					atomic.AddInt32(nudgeCalls, 1)
				}
			}
		}
		return &model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "This exact sentence repeats without variation. "},
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": fmt.Sprintf("file%d.go", n)}}},
			}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil
	}
}

// TestRunJudgeAgent_RepeatTripSkipsSubmitNudge: a repeat trip cancels runCtx like a forced close but without
// forcedVerdictCallback, so the nudge must skip it too.
func TestRunJudgeAgent_RepeatTripSkipsSubmitNudge(t *testing.T) {
	readTool := newSpyReadTool(t, "package x\n", new(int32))
	var calls, nudgeCalls int32
	judge := repeatTrippedJudgeModel(&calls, &nudgeCalls)
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	// Generous turn budget so a run to the turn cap (rather than the repeat
	// guard) would make the test fail loud, not pass by accident.
	cfg := Config{Rubric: "score 0-10", JudgeMaxIterations: 1000}

	_, _, err := runJudgeRound(t.Context(), factory, cfg, q, "done.", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrJudgeNoVerdict)", err)
	}
	if got := atomic.LoadInt32(&calls); got >= 1000 {
		t.Errorf("judge model called %d times, want well under the 1000-turn cap - the repeat guard should trip first", got)
	}
	if got := atomic.LoadInt32(&nudgeCalls); got != 0 {
		t.Errorf("judge model saw the submit_verdict nudge in %d request(s), want 0 - a repeat-trip abort must not nudge on the cancelled context", got)
	}
}

// inconsistentThenFixedJudge fails a criterion with no fix in round 1, then corrects the score in round 2.
func inconsistentThenFixedJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		n := atomic.AddInt32(calls, 1)
		score := 0.0
		if n > 1 {
			score = 3.0
		}
		return stubCall(submitVerdictTool, map[string]any{
			"score": score,
			"criteria": map[string]any{
				"verification_over_assertion": map[string]any{
					"score":     score,
					"shortfall": "All runs were warranted and stated.",
					"fix":       "",
				},
			},
			"feedback": "",
		}), nil
	}
}

// consistentFailJudge scores below threshold but gives a fix, exactly what
// the prompt asks for on a genuine failure - must never be re-asked.
func consistentFailJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		atomic.AddInt32(calls, 1)
		return stubCall(submitVerdictTool, map[string]any{
			"score": 1.0,
			"criteria": map[string]any{
				"verification_over_assertion": map[string]any{
					"score":     1.0,
					"shortfall": "trusted the description without reading the tests",
					"fix":       "read the tests and CI before scoring",
				},
			},
			"feedback": "",
		}), nil
	}
}

// requireFixOnFailSpecs: verification_over_assertion opted into the
// inconsistent-failure re-ask, matching .agents/plugins/github/agents/code-reviewer/rubric.yaml.
var requireFixOnFailSpecs = map[string]criterionSpec{
	"verification_over_assertion": {Name: "verification_over_assertion", RequireFixOnFail: true},
}

// TestFinishJudgeRound_ReasksOnInconsistentFailure: a criterion scored below
// threshold with no fix is re-judged once; the corrected score wins.
func TestFinishJudgeRound_ReasksOnInconsistentFailure(t *testing.T) {
	var calls int32
	judge := inconsistentThenFixedJudge(&calls)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review the change."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: requireFixOnFailSpecs}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("judge model called %d times, want 2 (original round + one re-ask)", got)
	}
	c, ok := v.Criteria["verification_over_assertion"]
	if !ok || c.Score != 1.0 {
		t.Fatalf("verification_over_assertion = %+v, want the re-judged score 1.0 (raw 3/3)", c)
	}
}

// TestFinishJudgeRound_NoReaskWhenFixGiven: a genuine failure (fix given)
// must never trigger the inconsistency re-ask.
func TestFinishJudgeRound_NoReaskWhenFixGiven(t *testing.T) {
	var calls int32
	judge := consistentFailJudge(&calls)
	factory := NewJudgeFactory(judge, nil, nil)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review the change."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: requireFixOnFailSpecs}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("judge model called %d times, want 1 (a fix given makes the fail consistent, no re-ask)", got)
	}
	c, ok := v.Criteria["verification_over_assertion"]
	if !ok || c.Score != 1.0/3.0 {
		t.Fatalf("verification_over_assertion = %+v, want the original raw 1/3 score", c)
	}
}

// TestInconsistentJudgeFailures: only an opted-in judge-scored fail with no fix qualifies.
func TestInconsistentJudgeFailures(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{
		"deterministic_fail": {Score: 0, Deterministic: true},
		"passing":            {Score: 0.9},
		"fail_with_fix":      {Score: 0.2, Fix: "do the thing"},
		"fail_no_fix":        {Score: 0.2, Shortfall: "looks fine actually"},
		"fail_not_opted_in":  {Score: 0.2, Shortfall: "committed unrequested work"},
	}}
	specs := map[string]criterionSpec{
		"fail_with_fix": {RequireFixOnFail: true},
		"fail_no_fix":   {RequireFixOnFail: true},
		// fail_not_opted_in has no spec entry: most criteria never require a named fix.
	}
	got := inconsistentJudgeFailures(v, 0.6, specs)
	if len(got) != 1 || got[0] != "fail_no_fix" {
		t.Fatalf("inconsistentJudgeFailures = %v, want [fail_no_fix]", got)
	}
}

// TestParseVerdict_MissingScoreIsNotAZero: an omitted criterion score and a non-rubric "score_note"
// must not parse as zeros that fail the verdict.
func TestParseVerdict_MissingScoreIsNotAZero(t *testing.T) {
	raw := `{"criteria":{"claims_grounded":{"reason":"all confirmed","score":3},` +
		`"verification_over_assertion":{"reason":"the top band"},` +
		`"score_note":{"corrected":"verification_over_assertion corrected to 3"}},"score":3}`
	v, err := parseVerdict(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := v.Criteria["verification_over_assertion"]; !c.Unscored {
		t.Errorf("a criterion with no score key must be marked unscored: %+v", c)
	}
	if c := v.Criteria["claims_grounded"]; c.Unscored {
		t.Errorf("a scored criterion must not be marked unscored: %+v", c)
	}
	specs := map[string]criterionSpec{"claims_grounded": {}, "verification_over_assertion": {}}
	v = dropUnscoredStrays(v, specs)
	if _, ok := v.Criteria["score_note"]; ok {
		t.Error("an unscored key the rubric does not define is noise and must be dropped")
	}
	if got := unscoredCriteria(v); len(got) != 1 || got[0] != "verification_over_assertion" {
		t.Errorf("unscoredCriteria = %v, want the rubric criterion the judge left unscored", got)
	}
}

// TestSubmitVerdictArgs_MissingScoreIsNotAZero: the submit_verdict tool decodes
// its arguments through the same criterion type, so it gets the same marking.
func TestSubmitVerdictArgs_MissingScoreIsNotAZero(t *testing.T) {
	var args verdictArgs
	if err := json.Unmarshal([]byte(`{"criteria":{"a":{"shortfall":"x"},"b":{"score":0}}}`), &args); err != nil {
		t.Fatal(err)
	}
	if !args.Criteria["a"].Unscored || args.Criteria["b"].Unscored {
		t.Errorf("unscored marking wrong: %+v", args.Criteria)
	}
}

// unscoredThenScoredJudge first answers with a rubric criterion that has no score plus a
// non-rubric aside, then scores it.
func unscoredThenScoredJudge(calls *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		crit := map[string]any{"shortfall": "Reading settled the claims."}
		extra := map[string]any{}
		if atomic.AddInt32(calls, 1) > 1 {
			crit["score"] = 3.0
		} else {
			extra["score_note"] = map[string]any{"corrected": "verification_over_assertion corrected to 3"}
		}
		criteria := map[string]any{"verification_over_assertion": crit}
		for k, v := range extra {
			criteria[k] = v
		}
		// Plain JSON text, as prod's judge answered: the submit_verdict tool's schema
		// would reject a missing score before the gate ever saw it.
		raw, _ := json.Marshal(map[string]any{"score": 3.0, "criteria": criteria, "feedback": ""})
		return stubText(string(raw)), nil
	}
}

func TestFinishJudgeRound_ReasksWhenACriterionIsUnscored(t *testing.T) {
	var calls int32
	judge := unscoredThenScoredJudge(&calls)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review the change."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6,
		RubricSpecs: map[string]criterionSpec{"verification_over_assertion": {Name: "verification_over_assertion"}}}

	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("judge model called %d times, want 2 (original round + one re-ask for the unscored criterion)", got)
	}
	if c := v.Criteria["verification_over_assertion"]; c.Unscored || c.Score != 1.0 {
		t.Errorf("verification_over_assertion = %+v, want the re-asked score 1.0 (raw 3/3)", c)
	}
	if _, ok := v.Criteria["score_note"]; ok {
		t.Error("the non-rubric aside must not survive as a criterion")
	}
}
