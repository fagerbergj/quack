package vetting

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// judgeReasoningProse mirrors a qwen3.8-27b pr-tutor judge's reasoning (QA chat
// c5c72e00 seq 59): it quotes stored surface JSON and diff code before any verdict.
const judgeReasoningProse = "### `surface_delivered`\n\nLet me check the structural elements:\n\n" +
	"1. Root: `{\"child\":\"main\",\"component\":\"Card\",\"id\":\"root\"}` -> Card with child \"main\"\n" +
	"2. `{\"tabs\":[{\"child\":\"overview\",\"title\":\"Overview\"},{\"child\":\"flow\",\"title\":\"Flow\"}]}` PASS\n" +
	"3. Guard: `-if worktreeValid(target, parentDir) {` becomes `+if worktreeValid(target, parentDir) && syncWorktree(ctx, target, parentDir, caps) {`\n" +
	"4. data_model: `{\"answers\":{\"q1\":[],\"q2\":[]}}` PASS\n\nAll criteria verified.\n\n"

const judgeReasoningVerdict = `{"score": 3, "criteria": {"answers_not_guessable": {"reason": "q3's keyed option is the shortest of its four", "score": 3}, ` +
	`"quiz_grounded": {"reason": "q2's why cites a comment the diff does not contain", "score": 2}}, "feedback": "Ground q2's why in the diff."}`

func TestParseVerdict_TakesLastCompleteVerdictObject(t *testing.T) {
	draft := `{"score": 1, "criteria": {"answers_not_guessable": {"reason": "draft", "score": 1}}, "feedback": "draft"}`
	for name, raw := range map[string]string{
		"reasoning quoting artifact JSON": judgeReasoningProse + judgeReasoningVerdict,
		"draft superseded by final":       judgeReasoningProse + draft + "\n\nOn reflection:\n\n" + judgeReasoningVerdict,
		"final then half-written redraft": judgeReasoningVerdict + "\n\nLet me restate it: " + `{"score": 1, "criteria": {"answers_not_guessable": {"score": 1}`,
		"fenced with trailing prose":      "```json\n" + judgeReasoningVerdict + "\n```\nThat is my verdict {as submitted}.",
		"criteria as a named list": `{"score": 3, "criteria": [{"name": "answers_not_guessable", "reason": "q3's keyed option is the shortest of its four", "score": 3}, ` +
			`{"name": "quiz_grounded", "reason": "q2's why cites a comment the diff does not contain", "score": 2}], "feedback": "Ground q2's why in the diff."}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, err := parseVerdict(raw, false)
			if err != nil {
				t.Fatalf("parseVerdict: %v", err)
			}
			if len(v.Criteria) != 2 || v.Criteria["quiz_grounded"].Score != 2.0/3.0 || v.Criteria["answers_not_guessable"].Score != 1 {
				t.Errorf("criteria = %+v, want the final verdict's two criteria", v.Criteria)
			}
			if v.Score != 2.0/3.0 || v.Feedback != "Ground q2's why in the diff." {
				t.Errorf("score/feedback = %v/%q, want the final verdict's", v.Score, v.Feedback)
			}
		})
	}
}

func TestParseVerdict_RejectsTextWithNoVerdictObject(t *testing.T) {
	for name, raw := range map[string]string{
		"only quoted artifact JSON":      judgeReasoningProse,
		"truncated draft at a token cap": judgeReasoningProse + `{"score": 1, "criteria": {"answers_not_guessable": {"reason": "x", "score": 1}`,
	} {
		if v, err := parseVerdict(raw, true); err == nil {
			t.Errorf("%s: parseVerdict = %+v, want an error so the round retries instead of scoring 0", name, v)
		}
	}
}

// replyJudge answers every call with the same parts and no tool call.
type replyJudge struct{ parts []*genai.Part }

func (j replyJudge) Name() string { return "reply-judge" }

func (j replyJudge) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: j.parts}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// A verdict written only in reasoning reaches the judge as a Thought part plus the
// inference layer's promoted copy of it (openaimodel applyFallbackLadder).
func TestRunJudgeAgent_VerdictPromotedFromReasoning(t *testing.T) {
	reasoning := judgeReasoningProse + judgeReasoningVerdict
	judge := replyJudge{parts: []*genai.Part{{Text: reasoning, Thought: true}, {Text: reasoning}}}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Build the tutor surface."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6}

	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if len(v.Criteria) != 2 || v.Feedback == "" {
		t.Errorf("verdict = %+v, want the reasoning's verdict with criteria and feedback, not a verdict-less 0", v)
	}
}

func TestRunJudgeAgent_NonVerdictJSONRoutesToNoVerdict(t *testing.T) {
	judge := replyJudge{parts: []*genai.Part{{Text: judgeReasoningProse}}}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Build the tutor surface."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6}

	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("runJudgeAgent = (%+v, %v), want ErrJudgeNoVerdict - quoted artifact JSON is not a verdict", v, err)
	}
}
