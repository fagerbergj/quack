package vetting

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// passJudge scores per judge session: the first pass's verdict, or the second opinion's (err fails it).
// Each session's first call waits for the other session, so a sequential second pass is caught.
type passJudge struct {
	first, second map[string]any
	secondErr     error
	barrier       bool

	mu       sync.Mutex
	calls    map[string]int
	started  int
	both     chan struct{}
	timedOut bool
}

func newPassJudge(first, second map[string]any) *passJudge {
	return &passJudge{first: first, second: second, calls: map[string]int{}, both: make(chan struct{})}
}

func (*passJudge) Name() string { return "pass-judge" }

func (p *passJudge) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	isSecond := strings.HasSuffix(ctx.(adkagent.InvocationContext).Session().ID(), "-second-opinion")
	p.mu.Lock()
	key := map[bool]string{false: "first", true: "second"}[isSecond]
	p.calls[key]++
	if p.calls[key] == 1 {
		if p.started++; p.started == 2 {
			close(p.both)
		}
	}
	p.mu.Unlock()
	if p.barrier {
		select {
		case <-p.both:
		case <-time.After(5 * time.Second):
			p.mu.Lock()
			p.timedOut = true
			p.mu.Unlock()
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case isSecond && p.secondErr != nil:
			yield(nil, p.secondErr)
		case isSecond:
			yield(stubCall(submitVerdictTool, p.second), nil)
		default:
			yield(stubCall(submitVerdictTool, p.first), nil)
		}
	}
}

func criteriaArgs(catches float64, catchesReason, otherReason string) map[string]any {
	return map[string]any{"score": catches, "feedback": "", "criteria": map[string]any{
		catchesRealIssuesCriterion: map[string]any{"score": catches, "reason": catchesReason, "fix": catchesReason},
		"clear_findings":           map[string]any{"score": 3, "reason": otherReason},
	}}
}

func stagedReview(event string) workerActivity {
	return workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: event}}}
}

func judgeReview(t *testing.T, j *passJudge, cfg Config, act workerActivity) (verdict, error) {
	t.Helper()
	q := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "review the PR"}}}
	return runJudgeAgent(t.Context(), NewJudgeFactory(j, nil, nil), cfg, q, "LGTM, approving.", act, nil, nil, func(*genai.Part) bool { return true })
}

func TestSecondOpinionTakesLowerCatchesRealIssues(t *testing.T) {
	cases := []struct {
		name        string
		first       float64
		second      float64
		wantScore   float64
		wantCatches string
	}{
		{"second lower wins", 3, 0, 0, "second: missed the nil deref"},
		{"first lower kept", 0, 3, 0, "first: missed it"},
		{"equal keeps first", 3, 3, 1, "first: missed it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newPassJudge(criteriaArgs(tc.first, "first: missed it", "first other"),
				criteriaArgs(tc.second, "second: missed the nil deref", "second other"))
			j.barrier = true
			cfg := Config{Rubric: "score 0-3", IsReviewer: true, judgeTiming: newJudgeRoundTiming()}
			v, err := judgeReview(t, j, cfg, stagedReview("approve"))
			if err != nil {
				t.Fatalf("runJudgeAgent: %v", err)
			}
			if j.timedOut {
				t.Fatal("the two judge passes did not run concurrently")
			}
			c := v.Criteria[catchesRealIssuesCriterion]
			if c.Score != tc.wantScore || c.Reason != tc.wantCatches || c.Fix != tc.wantCatches {
				t.Errorf("catches_real_issues = %+v, want score %v from %q", c, tc.wantScore, tc.wantCatches)
			}
			if other := v.Criteria["clear_findings"]; other.Reason != "first other" {
				t.Errorf("clear_findings = %+v, want the first pass's", other)
			}
			if cfg.judgeTiming.JudgeCalls != 2 {
				t.Errorf("JudgeCalls = %d, want both passes counted", cfg.judgeTiming.JudgeCalls)
			}
		})
	}
}

func TestSecondOpinionOnlyForApprovingReviewer(t *testing.T) {
	cases := []struct {
		name     string
		reviewer bool
		event    string
	}{
		{"request_changes", true, "request_changes"},
		{"comment", true, "comment"},
		{"nothing staged", true, ""},
		{"non-reviewer approve", false, "approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newPassJudge(criteriaArgs(3, "ok", "ok"), criteriaArgs(0, "no", "no"))
			if _, err := judgeReview(t, j, Config{Rubric: "score 0-3", IsReviewer: tc.reviewer}, stagedReview(tc.event)); err != nil {
				t.Fatalf("runJudgeAgent: %v", err)
			}
			if j.calls["first"] != 1 || j.calls["second"] != 0 {
				t.Errorf("judge calls = %v, want exactly one first pass", j.calls)
			}
		})
	}
}

func TestSecondOpinionErrorKeepsFirstPass(t *testing.T) {
	j := newPassJudge(criteriaArgs(3, "first: clean", "first other"), nil)
	j.secondErr = errors.New("status 400: bad request")
	v, err := judgeReview(t, j, Config{Rubric: "score 0-3", IsReviewer: true}, stagedReview("approve"))
	if err != nil {
		t.Fatalf("runJudgeAgent = %v, want the first pass to stand when the second opinion fails", err)
	}
	if c := v.Criteria[catchesRealIssuesCriterion]; c.Score != 1 || c.Reason != "first: clean" {
		t.Errorf("catches_real_issues = %+v, want the first pass's", c)
	}
	if j.calls["second"] == 0 {
		t.Error("second opinion never ran")
	}
}

func TestReplayRoundRunsSecondOpinionOnApprove(t *testing.T) {
	j := newPassJudge(criteriaArgs(3, "first: clean", "first other"), criteriaArgs(0, "second: missed it", "second other"))
	stage := NewReviewStage(nil)
	if err := stage.SetVerdict("approve", "lgtm", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	rc := ReplayCase{NodeID: "n1", Task: "review the PR", Answer: "LGTM, approving.", Review: stage}
	res, err := ReplayRound(t.Context(), Config{Threshold: 0.5, IsReviewer: true}, NewJudgeFactory(j, nil, nil), rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	for _, c := range res.Criteria {
		if c.Name == catchesRealIssuesCriterion && c.Score == 0 {
			return
		}
	}
	t.Errorf("criteria %+v: want the second opinion's failing catches_real_issues", res.Criteria)
}
