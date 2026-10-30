package vetting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/ledger"
)

// scoreSrv answers the score question with level 2. A non-nil gate holds each request until it is closed;
// states receives every request's state; maxIn is the most requests ever in flight at once.
type scoreSrv struct {
	URL    string
	states chan json.RawMessage
	gate   chan struct{}
	delay  time.Duration
	in     atomic.Int32
	maxIn  atomic.Int32
}

func newScoreSrv(t *testing.T, gate chan struct{}, delay time.Duration) *scoreSrv {
	t.Helper()
	s := &scoreSrv{states: make(chan json.RawMessage, 64), gate: gate, delay: delay}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.in.Add(1)
		defer s.in.Add(-1)
		for m := s.maxIn.Load(); n > m && !s.maxIn.CompareAndSwap(m, n); m = s.maxIn.Load() {
		}
		var body struct{ State json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.states <- body.State
		if s.gate != nil {
			select {
			case <-s.gate:
			case <-r.Context().Done():
				return
			}
		}
		time.Sleep(s.delay)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"score": map[string]any{"type": "score", "probabilities": map[string]float64{"1": 0.2, "2": 0.8}}}})
	}))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func TestObserveCriteriaRecordsEachCriterionWithMeta(t *testing.T) {
	mem := decidetest.Capture(t)
	srv := newScoreSrv(t, nil, 0)
	j := criteriaRounds(decidetest.Decider(t, srv.URL, judgeCriteria.ID))
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-jc", Node: "n1", Agent: "web-researcher"})
	j.observeCriteria(ctx, 2, criteriaVerdict())
	got := decidetest.Records(t, mem, "chat-jc", 2)
	if len(got) != 2 || len(srv.states) != 2 {
		t.Fatalf("records = %d, requests = %d, want 2 each", len(got), len(srv.states))
	}
	byCriterion := map[string]ledger.DecisionPayload{}
	for _, r := range got {
		var m struct {
			Agent, Criterion string
			Round            int
			GradeWords       bool `json:"grade_words"`
		}
		_ = json.Unmarshal(r.Meta, &m)
		if r.Point != "judge.criteria" || r.Top != "2" || m.Agent != "web-researcher" || m.Round != 2 || m.GradeWords || strings.Contains(string(r.State), "meta") {
			t.Errorf("record = %+v meta %s, want a judge.criteria answer 2 with agent, round and grade_words=false in meta only", r, r.Meta)
		}
		byCriterion[m.Criterion] = r
	}
	if byCriterion["grounded"].Baseline != "2" || byCriterion["complete"].Baseline != "3" {
		t.Errorf("baselines = grounded %q complete %q, want 2 and 3", byCriterion["grounded"].Baseline, byCriterion["complete"].Baseline)
	}
}

// TestObserveCriteriaNeverBlocksTheGate holds the handler mid-request: the caller must already have returned.
func TestObserveCriteriaNeverBlocksTheGate(t *testing.T) {
	mem := decidetest.Capture(t)
	gate := make(chan struct{})
	srv := newScoreSrv(t, gate, 0)
	j := criteriaRounds(decidetest.Decider(t, srv.URL, judgeCriteria.ID))
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-block", Node: "n1"})
	returned := make(chan struct{})
	go func() {
		j.observeCriteria(ctx, 1, criteriaVerdict())
		close(returned)
	}()
	select {
	case <-srv.states: // the handler holds a request open
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the handler")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("observeCriteria did not return while the handler was holding a request")
	}
	close(gate)
	if got := decidetest.Records(t, mem, "chat-block", 2); len(got) != 2 {
		t.Errorf("records = %d after release, want 2", len(got))
	}
}

// TestObserveCriteriaDisabledDoesNoWork fails without the Enabled guard: no state is built and no request is sent.
func TestObserveCriteriaDisabledDoesNoWork(t *testing.T) {
	srv := newScoreSrv(t, nil, 0)
	j := criteriaRounds(decidetest.Decider(t, srv.URL, answerAccept.ID))
	before := judgeCriteriaBuilds.Load()
	j.observeCriteria(context.Background(), 1, criteriaVerdict())
	time.Sleep(50 * time.Millisecond)
	if n := judgeCriteriaBuilds.Load() - before; n != 0 || len(srv.states) != 0 {
		t.Errorf("states built = %d, requests = %d, want 0 with judge.criteria off", n, len(srv.states))
	}
}

// TestObserveCriteriaIsSingleFlight: concurrent rounds share bulkLane, so the handler never holds two asks at once.
func TestObserveCriteriaIsSingleFlight(t *testing.T) {
	mem := decidetest.Capture(t)
	srv := newScoreSrv(t, nil, 30*time.Millisecond)
	d := decidetest.Decider(t, srv.URL, judgeCriteria.ID)
	var wg sync.WaitGroup
	for _, chat := range []string{"chat-sf1", "chat-sf2", "chat-sf3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			criteriaRounds(d).observeCriteria(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chat, Node: "n"}), 1, criteriaVerdict())
		}()
	}
	wg.Wait()
	for _, chat := range []string{"chat-sf1", "chat-sf2", "chat-sf3"} {
		if got := decidetest.Records(t, mem, chat, 2); len(got) != 2 {
			t.Fatalf("%s records = %d, want 2", chat, len(got))
		}
	}
	if m := srv.maxIn.Load(); m != 1 {
		t.Errorf("max in-flight asks = %d, want 1", m)
	}
}

func TestJudgeCriteriaBaselineFloorsTheUnitScore(t *testing.T) {
	for _, c := range []struct {
		unit float64
		want int
	}{{0, 0}, {1.0 / 3, 1}, {0.5, 1}, {2.0 / 3, 2}, {0.833, 2}, {1, 3}} {
		if got := levelOf(c.unit); got != c.want {
			t.Errorf("levelOf(%v) = %d, want %d (a half level must not read as the next, passing one)", c.unit, got, c.want)
		}
	}
}

func TestJudgeCriteriaMetaFlagsGradeWords(t *testing.T) {
	for text, want := range map[string]bool{
		"read a.go:10; the claim matches line 12": false,
		"This passes the rubric":                  true,
		"Falls in the 2-3 band":                   true,
		"No test exists; the check fails":         true,
		"compass bypassed":                        false,
	} {
		if got := gradeWords.MatchString(text); got != want {
			t.Errorf("gradeWords(%q) = %v, want %v", text, got, want)
		}
	}
	v := criteriaVerdict()
	c := v.Criteria["complete"]
	c.Checked = "All three asks are answered, so it meets the band."
	v.Criteria["complete"] = c
	for _, a := range criteriaRounds(nil).judgeCriteriaAsks(1, v) {
		flag := a.state.Meta.(map[string]any)["grade_words"]
		if want := a.state.State.(judgeCriteriaState).Criterion == "complete"; flag != want {
			t.Errorf("%s grade_words = %v, want %v", a.state.State.(judgeCriteriaState).Criterion, flag, want)
		}
	}
}

// TestJudgeCriteriaSkipsCriterionTheFanoutSliceDrops: a fan-out slice is never judged on structured_verdict.
func TestJudgeCriteriaSkipsCriterionTheFanoutSliceDrops(t *testing.T) {
	fo := GetReviewFanout("plan-jc", 2)
	fo.ExpectSynthesis()
	t.Cleanup(func() { ResetReviewFanout("plan-jc") })
	j := criteriaRounds(nil)
	j.cfg.IsReviewer, j.cfg.ReviewFanout = true, fo
	j.cfg.RubricSpecs = map[string]criterionSpec{"structured_verdict": criteriaSpecs["grounded"], "grounded": criteriaSpecs["grounded"]}
	v := verdict{Criteria: map[string]criterionScore{"structured_verdict": {Score: 1, Checked: "x"}, "grounded": {Score: 1, Checked: "y"}}}
	asks := j.judgeCriteriaAsks(1, v)
	if len(asks) != 1 || asks[0].state.State.(judgeCriteriaState).Criterion != "grounded" {
		t.Errorf("asks = %+v, want only grounded for a non-delivering slice", asks)
	}
}
