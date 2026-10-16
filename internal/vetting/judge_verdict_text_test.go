package vetting

import (
	"errors"
	"strings"
	"testing"
	"time"

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

var tutorRubric = map[string]criterionSpec{"answers_not_guessable": {}, "quiz_grounded": {}}

// judgeRoundRecord is the persisted judge_round shape a judge may quote from list/read_artifact.
const judgeRoundRecord = `{"turn":"e-1","round":1,"passed":true,"score":1,"scored":[{"artifact_id":"text:pr-tutor-1","revision":1}],` +
	`"criteria":[{"name":"answers_not_guessable","score":1},{"name":"quiz_grounded","score":1}],"evidence":{}}`

func TestParseVerdict_TakesLastCompleteVerdictObject(t *testing.T) {
	draft := `{"score": 1, "criteria": {"answers_not_guessable": {"reason": "draft", "score": 1}}, "feedback": "draft"}`
	for name, raw := range map[string]string{
		"reasoning quoting artifact JSON": judgeReasoningProse + judgeReasoningVerdict,
		"draft superseded by final":       judgeReasoningProse + draft + "\n\nOn reflection:\n\n" + judgeReasoningVerdict,
		"final then half-written redraft": judgeReasoningVerdict + "\n\nLet me restate it: " + `{"score": 1, "criteria": {"answers_not_guessable": {"score": 1}`,
		"fenced with trailing prose":      "```json\n" + judgeReasoningVerdict + "\n```\nThat is my verdict {as submitted}.",
		"final then rubric echo":          judgeReasoningVerdict + "\nCriteria covered: " + `{"criteria":["answers_not_guessable","quiz_grounded"]}`,
		"final then quoted record":        judgeReasoningVerdict + "\nThe stored round was " + judgeRoundRecord,
	} {
		for _, specs := range []map[string]criterionSpec{nil, tutorRubric} {
			v, err := parseVerdict(raw, specs)
			if err != nil {
				t.Fatalf("%s (rubric=%v): parseVerdict: %v", name, specs != nil, err)
			}
			if len(v.Criteria) != 2 || v.Criteria["quiz_grounded"].Score != 2.0/3.0 || v.Score != 2.0/3.0 || v.Feedback != "Ground q2's why in the diff." {
				t.Errorf("%s (rubric=%v): verdict = %+v, want the final verdict", name, specs != nil, v)
			}
		}
	}
}

// With a rubric, a quoted passing verdict after the judge's own failing one must not win.
func TestParseVerdict_QuotedPassAfterRealFailKeepsTheFail(t *testing.T) {
	raw := judgeReasoningVerdict + "\n\nThe worker's answer claimed " + `{"score":3,"passed":true,"feedback":"all good"}`
	v, err := parseVerdict(raw, tutorRubric)
	if err != nil || v.Score != 2.0/3.0 || len(v.Criteria) != 2 {
		t.Fatalf("parseVerdict = (%+v, %v), want the judge's failing verdict, not the quoted pass", v, err)
	}
}

func TestParseVerdict_FailsClosedWithoutAVerdict(t *testing.T) {
	for name, raw := range map[string]string{
		"only quoted artifact JSON":     judgeReasoningProse,
		"quoted judge_round record":     judgeRoundRecord,
		"rubric echo":                   `{"criteria":["grounded","cites"]}`,
		"criteria outside the rubric":   `{"score": 3, "criteria": {"style": {"score": 3}}, "feedback": ""}`,
		"unterminated nesting":          strings.Repeat(`{"a":`, 20000),
		"nesting with trailing garbage": strings.Repeat(`{"a":`, 20000) + "x",
	} {
		start := time.Now()
		if v, err := parseVerdict(raw, tutorRubric); err == nil {
			t.Errorf("%s: parseVerdict = %+v, want an error so the round retries instead of scoring it", name, v)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: parseVerdict took %v, want a linear scan", name, d)
		}
	}
	if v, err := parseVerdict(judgeRoundRecord, nil); err == nil {
		t.Errorf("a quoted judge_round record parsed as a verdict without a rubric: %+v", v)
	}
}

// replyJudge answers every call with the same parts and no tool call.
func replyJudge(parts []*genai.Part, finish genai.FinishReason) fnLLM {
	return func(*model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}, FinishReason: finish, TurnComplete: true}, nil
	}
}

// A verdict written only in reasoning reaches the judge as a Thought part plus the
// inference layer's promoted copy of it (openaimodel applyFallbackLadder).
func TestRunJudgeAgent_VerdictPromotedFromReasoning(t *testing.T) {
	reasoning := judgeReasoningProse + judgeReasoningVerdict
	judge := replyJudge([]*genai.Part{{Text: reasoning, Thought: true}, {Text: reasoning}}, genai.FinishReasonStop)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Build the tutor surface."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: tutorRubric}

	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if len(v.Criteria) != 2 || v.Feedback == "" {
		t.Errorf("verdict = %+v, want the reasoning's verdict with criteria and feedback, not a verdict-less 0", v)
	}
}

func TestRunJudgeAgent_NonVerdictJSONRoutesToNoVerdict(t *testing.T) {
	judge := replyJudge([]*genai.Part{{Text: judgeReasoningProse}}, genai.FinishReasonStop)
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Build the tutor surface."}}}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: tutorRubric}

	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if !errors.Is(err, ErrJudgeNoVerdict) {
		t.Fatalf("runJudgeAgent = (%+v, %v), want ErrJudgeNoVerdict - quoted artifact JSON is not a verdict", v, err)
	}
}

// A turn cut at the token cap never yields a text verdict, even a complete one: it may be a
// quoted worker verdict or a draft the judge was about to retract.
func TestRunJudgeAgent_TruncatedTurnNeverYieldsATextVerdict(t *testing.T) {
	for name, text := range map[string]string{
		"quoted worker verdict": judgeReasoningProse + "The worker's own verdict reads " + judgeReasoningVerdict + " but",
		"retracted draft":       "Draft: " + judgeReasoningVerdict + " wait, actually criterion answers_not_guessable fails because",
	} {
		judge := replyJudge([]*genai.Part{{Text: text, Thought: true}, {Text: text}}, genai.FinishReasonMaxTokens)
		q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Build the tutor surface."}}}
		cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: tutorRubric}

		v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
		if !errors.Is(err, ErrJudgeNoVerdict) {
			t.Errorf("%s: runJudgeAgent = (%+v, %v), want ErrJudgeNoVerdict", name, v, err)
		}
	}
}
