// judgeround_test.go: #1092 - judge_round record, trigger_annotation chain,
// notes anchoring, revise-prompt sourcing, and fan-out verdict ownership.
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

func judgeRoundID(turnID string, round int) string {
	id, err := recordstore.IdentityFor(kindJudgeRound, nil, fmt.Sprintf("%s-%d", turnID, round))
	if err != nil {
		panic(err)
	}
	return id
}

// TestJudgeRoundRecordTriggerAnnotationChain is design V4 §7 case 3: every
// judge round writes a revision with parent_revision and trigger_annotation
// set, and exactly one judge_round record whose "scored" matches the
// revisions it judged.
func TestJudgeRoundRecordTriggerAnnotationChain(t *testing.T) {
	svc := newMetaAwareInMemory()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	turnID := "t1"

	answer1 := `VERDICT: request_changes
FINDINGS:
- a.go:1: bug one. it breaks things
CLEAN:
`
	st := newEpisodicRoundState()
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, turnID, 1, answer1, StagedDelivery{Kind: "review", Recovered: true}, st)
	scored1 := append([]ScoredRef(nil), st.roundWrites...)
	if len(scored1) == 0 {
		t.Fatal("round 1 wrote nothing")
	}
	v1 := verdict{Score: 0.3, Passed: false, Criteria: map[string]criterionScore{
		"catches_real_issues": {Score: 0.3, Shortfall: "missed something"},
	}}
	rec1 := buildJudgeRoundRecord(turnID, 1, false, 0.3, scored1, v1, nil, answer1)
	jr1ID, jr1Rev := saveJudgeRoundRecord(context.Background(), cfg, cfg.NodeID, turnID, 1, rec1)
	if jr1ID == "" || jr1Rev != 1 {
		t.Fatalf("round 1 judge_round save: id=%q rev=%d", jr1ID, jr1Rev)
	}
	if jr1ID != judgeRoundID(turnID, 1) {
		t.Fatalf("judge_round id = %q, want %q", jr1ID, judgeRoundID(turnID, 1))
	}
	st.triggerAnnotation = jr1ID // node.go's round-loop wiring

	// Round 2: the same code_review id gets a new revision whose lineage
	// carries parent_revision (from st.reviewRev, set by round 1's save) and
	// trigger_annotation = round 1's judge_round id.
	answer2 := `VERDICT: approve
FINDINGS:
CLEAN:
- a.go
`
	saveCodeReviewRound(context.Background(), cfg, cfg.NodeID, turnID, 2, answer2, StagedDelivery{Kind: "review", Recovered: true}, st)

	rc := recordClient(cfg)
	_, lineage, rev2, ok, err := rc.LatestWithMeta(context.Background(), codeReviewID(cfg))
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta round 2: ok=%v err=%v", ok, err)
	}
	if rev2 != 2 {
		t.Fatalf("code_review revision = %d, want 2", rev2)
	}
	if lineage.ParentRevision != 1 {
		t.Fatalf("lineage.ParentRevision = %d, want 1", lineage.ParentRevision)
	}
	if lineage.TriggerAnnotation != jr1ID {
		t.Fatalf("lineage.TriggerAnnotation = %q, want round 1's judge_round id %q", lineage.TriggerAnnotation, jr1ID)
	}

	scored2 := append([]ScoredRef(nil), st.roundWrites...)
	v2 := verdict{Score: 1, Passed: true, Criteria: map[string]criterionScore{"catches_real_issues": {Score: 1}}}
	rec2 := buildJudgeRoundRecord(turnID, 2, true, 1, scored2, v2, nil, answer2)
	jr2ID, _ := saveJudgeRoundRecord(context.Background(), cfg, cfg.NodeID, turnID, 2, rec2)
	if jr2ID == jr1ID {
		t.Fatal("round 2's judge_round id must differ from round 1's")
	}

	// Exactly one judge_round record per round, and its "scored" matches what
	// that round actually wrote.
	raw, _, _, ok, err := rc.LatestWithMeta(context.Background(), jr1ID)
	if err != nil || !ok {
		t.Fatalf("load round 1 judge_round: ok=%v err=%v", ok, err)
	}
	var loaded JudgeRoundRecord
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Scored) != len(scored1) {
		t.Fatalf("round 1 judge_round.Scored = %+v, want %+v", loaded.Scored, scored1)
	}
	for i, want := range scored1 {
		if loaded.Scored[i] != want {
			t.Fatalf("round 1 judge_round.Scored[%d] = %+v, want %+v", i, loaded.Scored[i], want)
		}
	}
}

// TestJudgeRoundRecordNotesAnchoring: a quote-anchored criterion becomes a
// Note referencing the round's scored artifact, with a best-effort LineHint
// found by searching the answer for the quoted text.
func TestJudgeRoundRecordNotesAnchoring(t *testing.T) {
	answer := "line one\nthe bug is right here\nline three"
	scored := []ScoredRef{{ArtifactID: "code_review:pr:1", Revision: 3}}
	v := verdict{Criteria: map[string]criterionScore{
		"catches_real_issues": {
			Score:     0,
			Shortfall: "misses the real defect",
			Anchor:    &anchorSpec{Kind: "quote", Text: "the bug is right here"},
		},
	}}
	rec := buildJudgeRoundRecord("t1", 1, false, 0, scored, v, nil, answer)
	if len(rec.Notes) != 1 {
		t.Fatalf("notes = %+v, want 1", rec.Notes)
	}
	n := rec.Notes[0]
	if n.Ref.ArtifactID != "code_review:pr:1" || n.Ref.Revision != 3 {
		t.Fatalf("note ref = %+v, want the round's scored artifact", n.Ref)
	}
	if n.Ref.Snippet != "the bug is right here" {
		t.Fatalf("note snippet = %q", n.Ref.Snippet)
	}
	if n.Ref.LineHint != 2 {
		t.Fatalf("note line_hint = %d, want 2", n.Ref.LineHint)
	}
	if n.Criterion != "catches_real_issues" {
		t.Fatalf("note criterion = %q", n.Criterion)
	}
}

// TestRevisePromptCarriesNoteRefs: the revise prompt built from a
// judge_round record's notes names the exact artifact_id/revision a worker
// would read_artifact/edit_artifact to address the feedback (#1092 scope
// item 3 - the prompt sources from the record, not ad hoc string-building).
func TestRevisePromptCarriesNoteRefs(t *testing.T) {
	notes := []JudgeNote{{
		Ref:       NoteRef{ArtifactID: "code_review:pr:42", Revision: 5, Snippet: "the bug is right here", LineHint: 2},
		Text:      "misses the real defect",
		Criterion: "catches_real_issues",
	}}
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review this PR"}}}
	env := verdictEnvelope{Passed: false, Score: 0.3, Threshold: 0.7, Round: 1}
	got := contentPlainText(buildRevisionContent("", question, "the answer", env, workerActivity{}, false, notes))

	for _, want := range []string{`"code_review:pr:42"`, `"revision": 5`, "the bug is right here"} {
		if !strings.Contains(got, want) {
			t.Fatalf("revise prompt missing %q; got:\n%s", want, got)
		}
	}
}

// TestNonDeliveringSliceDropsStructuredVerdict: a fan-out slice reviewer
// (ReviewFanout with a synthesizer expected) is never judged on
// structured_verdict, so a low score there doesn't sink the round.
func TestNonDeliveringSliceDropsStructuredVerdict(t *testing.T) {
	fo := GetReviewFanout("plan-x", 2)
	fo.ExpectSynthesis()
	t.Cleanup(func() { ResetReviewFanout("plan-x") })
	cfg := Config{IsReviewer: true, ReviewFanout: fo}
	if !isNonDeliveringSlice(cfg) {
		t.Fatal("expected a reviewer node in a synth-expected fan-out to be a non-delivering slice")
	}
	v := verdict{Criteria: map[string]criterionScore{
		"catches_real_issues": {Score: 1},
		"structured_verdict":  {Score: 0}, // would otherwise sink weakest-link
	}}
	got := dropCriteria(v, "structured_verdict")
	if _, ok := got.Criteria["structured_verdict"]; ok {
		t.Fatal("structured_verdict should have been dropped")
	}
	if got.Score != 1 {
		t.Fatalf("score = %v, want 1 (weakest-link over remaining criteria only)", got.Score)
	}
}

// TestMergeReviewsSynthesizerOwnsVerdict: the synthesizer's own VERDICT tail
// is the merge's verdict, overriding the worst-of computed from slices whose
// VERDICT lines should have been ignored (#1092, design V4 §4.6) - a
// slice-only approve doesn't force delivery to approve over the
// synthesizer's request_changes.
func TestMergeReviewsSynthesizerOwnsVerdict(t *testing.T) {
	terminal := map[string]reviewFanoutEntry{
		"slice-a": {ok: true, item: StagedDelivery{Kind: "review", Event: "approve"}},
		"slice-b": {ok: true, item: StagedDelivery{Kind: "review", Event: "approve"}},
	}
	synthBody := "Consolidated review.\n\nVERDICT: request_changes\n"
	merged := mergeReviews(terminal, synthBody)
	if merged.Event != "request_changes" {
		t.Fatalf("merged verdict = %q, want request_changes (synthesizer-owned, not slices' worst-of)", merged.Event)
	}
}

// TestMergeReviewsSlicesOnlyNitsPassesAsComment: a fan-out with only nits and
// every slice staging VERDICT: comment merges to comment when the
// synthesizer produces none (fallback path) - and a non-delivering slice
// with no blocking findings must not itself force a request_changes.
func TestMergeReviewsSlicesOnlyNitsPassesAsComment(t *testing.T) {
	terminal := map[string]reviewFanoutEntry{
		"slice-a": {ok: true, item: StagedDelivery{Kind: "review", Event: "comment"}},
	}
	merged := mergeReviews(terminal, "")
	if merged.Event != "comment" {
		t.Fatalf("merged verdict = %q, want comment", merged.Event)
	}
}

// TestNonDeliveringSliceStagesNoReview: a fan-out slice reviewer that only
// found nits and staged VERDICT: comment never delivers its own review - its
// item is consumed by ReviewFanout.Finish, not handed to cfg.Deliver
// (design V4 §4.6: a slice's VERDICT is staged only as delivery's
// worst-of-fallback input, never delivered on its own).
func TestNonDeliveringSliceStagesNoReview(t *testing.T) {
	var deliverCalls int
	deliver := func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		deliverCalls++
		return nil, nil
	}
	fo := GetReviewFanout("plan-nits", 2)
	fo.ExpectSynthesis()
	t.Cleanup(func() { ResetReviewFanout("plan-nits") })
	cfg := Config{Deliver: deliver, ReviewFanout: fo, IsReviewer: true}

	commitDelivery(context.Background(), nil, cfg, "slice-a", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "comment",
			Comments: []ReviewComment{{Path: "a.go", Line: 1, Body: "nit: rename this"}}}},
	}, GateResult{Passed: true})

	if deliverCalls != 0 {
		t.Fatalf("Deliver called %d times; a non-delivering slice must never deliver on its own", deliverCalls)
	}
}

// TestArtifactSSEEventOrder: within a round, artifact_revision fires for
// every revision the round wrote, all before that round's
// artifact_judge_round event (#1090 §4.8 - the judge_round record references
// revisions that must already be visible to a client).
func TestArtifactSSEEventOrder(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
		Artifact: kindText, ChatID: "chat1", User: "u1",
		Artifacts: newMetaAwareInMemory(),
	}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	var mu sync.Mutex
	var names []string
	ctx := stream.WithYield(t.Context(), func(ev stream.SSEEvent) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, ev.Name)
	})
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	for _, err := range r.Run(ctx, "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var judgeRounds int
	sawRevisionSinceLastJudgeRound := false
	for _, n := range names {
		switch n {
		case stream.EventArtifactRevision:
			sawRevisionSinceLastJudgeRound = true
		case stream.EventArtifactJudgeRound:
			if !sawRevisionSinceLastJudgeRound {
				t.Fatal("artifact_judge_round with no preceding artifact_revision this round")
			}
			sawRevisionSinceLastJudgeRound = false
			judgeRounds++
		}
	}
	if judgeRounds != 2 {
		t.Fatalf("artifact_judge_round events = %d, want 2 (fail then pass)", judgeRounds)
	}
}
