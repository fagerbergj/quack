package vetting

import (
	"context"
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
// calls each time the judge invokes it — proving the judge actually opened the
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
// the changed file, then — once it has the body back — submits a verdict whose
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
			factory := NewJudgeFactory(scriptedJudge{}, []tool.Tool{readTool})
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

// oneShotJudge submits a verdict on the first turn without calling any tool —
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
	factory := NewJudgeFactory(oneShotJudge{}, nil)
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
	with := judgeBehaviour(true)
	if !strings.Contains(with, "read-only workspace tools") || strings.Contains(with, "You have no tools") {
		t.Errorf("read-tools behaviour missing its clause: %q", with)
	}
	without := judgeBehaviour(false)
	if !strings.Contains(without, "You have no tools") || strings.Contains(without, "read-only workspace tools") {
		t.Errorf("no-tools behaviour missing its clause: %q", without)
	}
}
