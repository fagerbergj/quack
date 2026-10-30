package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
