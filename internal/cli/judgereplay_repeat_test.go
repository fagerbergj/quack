package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// passModel varies per call: pass N gives evidence "ev-N", N*10 output tokens and score N.
type passModel struct{ calls *int }

func (passModel) Name() string { return "pass-model" }

func (m passModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	*m.calls++
	n := *m.calls
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				Name: "submit_verdict", Args: map[string]any{"score": n, "feedback": "", "criteria": map[string]any{
					"answers_question": map[string]any{"score": n, "evidence": "ev-" + strconv.Itoa(n)}}},
			}}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: int32(10 * n)},
		}, nil)
	}
}

// TestRunJudgeReplayRepeatReportsEveryPass: --repeat 2 carries both passes' cost, score, evidence and point
// answer, not only the first pass's.
func TestRunJudgeReplayRepeatReportsEveryPass(t *testing.T) {
	var asks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probs := map[string]float64{"1": 0.7, "2": 0.3} // the first ask answers 1, the second 2
		if asks.Add(1) == 2 {
			probs = map[string]float64{"1": 0.3, "2": 0.7}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"score": map[string]any{"type": "score", "probabilities": probs}}})
	}))
	t.Cleanup(srv.Close)
	cfg := fixtureConfig()
	cfg.Decisions = decisionsFor(srv.URL, true)
	sess, err := bundle.Load(buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0}))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var buf bytes.Buffer
	RunJudgeReplay(context.Background(), cfg, sess, ReplayOptions{RubricPath: judgedRubric(t), Repeat: 2}, vetting.NewJudgeFactory(passModel{&calls}, nil, nil), nil, false, &buf, true)
	rep := decodeReports(t, &buf)[0]
	if len(rep.Passes) != 2 {
		t.Fatalf("passes = %d, want 2 (one per --repeat pass): %+v", len(rep.Passes), rep.Passes)
	}
	for i, p := range rep.Passes {
		n := i + 1
		if p.Judge == nil || p.Judge.OutputTokens != 10*n || len(p.Criteria) != 1 {
			t.Fatalf("pass %d = %+v, want %d output tokens and one judged criterion", n, p, 10*n)
		}
		c := p.Criteria[0]
		if c.Evidence != "ev-"+strconv.Itoa(n) || c.Point == nil || c.Point.Top != strconv.Itoa(n) || c.Score < float64(n)/3-0.001 || c.Score > float64(n)/3+0.001 {
			t.Errorf("pass %d criterion = %+v point %+v, want evidence ev-%d, score %d/3 and point answer %d", n, c, c.Point, n, n, n)
		}
	}
	if rep.Judge == nil || rep.Judge.OutputTokens != 10 {
		t.Errorf("summary judge = %+v, want the first pass's cost kept for current consumers", rep.Judge)
	}
}

// failingPassModel verdicts on call 1, then on call 2 spends a 20-token tool turn and on call 3 fails hard.
type failingPassModel struct{ calls *int }

func (failingPassModel) Name() string { return "failing-pass-model" }

func (m failingPassModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	*m.calls++
	n := *m.calls
	return func(yield func(*model.LLMResponse, error) bool) {
		call, usage := "submit_verdict", int32(10)
		if n == 2 {
			call, usage = "list_artifacts", 20
		}
		if n == 3 {
			yield(nil, errors.New("status 400: bad request"))
			return
		}
		args := map[string]any{}
		if n == 1 {
			args = map[string]any{"score": 3, "feedback": ""}
		}
		yield(&model.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: call, Args: args}}}},
			FinishReason:  genai.FinishReasonStop,
			TurnComplete:  true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: usage},
		}, nil)
	}
}

// TestRunJudgeReplayRepeatKeepsFailedPassCost: a --repeat pass that fails keeps the passes before it and is
// itself reported with its error and what it spent.
func TestRunJudgeReplayRepeatKeepsFailedPassCost(t *testing.T) {
	sess, err := bundle.Load(buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0}))
	if err != nil {
		t.Fatal(err)
	}
	stub, err := StubArtifactTools()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var buf bytes.Buffer
	code := RunJudgeReplay(context.Background(), fixtureConfig(), sess, ReplayOptions{RubricPath: judgedRubric(t), Repeat: 2}, vetting.NewJudgeFactory(failingPassModel{&calls}, nil, nil), stub, false, &buf, true)
	rep := decodeReports(t, &buf)[0]
	if code != 1 || rep.Spread != nil || len(rep.Passes) != 2 {
		t.Fatalf("exit %d, spread %+v, passes %+v; want exit 1, no spread and both passes", code, rep.Spread, rep.Passes)
	}
	ok, failed := rep.Passes[0], rep.Passes[1]
	if ok.Error != "" || ok.Judge == nil || ok.Judge.OutputTokens != 10 {
		t.Errorf("pass 1 = %+v, want a clean pass costing 10 output tokens", ok)
	}
	if failed.Error == "" || failed.Judge == nil || failed.Judge.OutputTokens != 20 || failed.Judge.Seconds <= 0 {
		t.Errorf("pass 2 = %+v, want its error and the 20 tokens and time it spent", failed)
	}
}
