package vetting

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// commitDelivery is the sole enforcement point: an ungranted review never reaches Deliver and
// surfaces as a failed delivery_result.
func TestCommitDeliveryRefusesUngrantedReview(t *testing.T) {
	var called int32
	cfg := Config{
		AllowedDeliveryKinds: []string{"pull_request"}, // no review
		Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
			called++
			return nil, nil
		},
	}
	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }

	commitDelivery(context.Background(), sink, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve"}},
	}, GateResult{Passed: true})

	if called != 0 {
		t.Fatalf("Deliver was called %d times, want 0 - the review was never granted", called)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one delivery_result", events)
	}
	data, ok := events[0].Data.(stream.DeliveryResultData)
	if !ok {
		t.Fatalf("event data = %T, want DeliveryResultData", events[0].Data)
	}
	if data.Outcome != stream.DeliveryOutcomeFailed || data.Kind != "review" || data.Error == "" {
		t.Fatalf("delivery_result = %+v, want a failed review outcome naming the refusal", data)
	}
}

// A staged review with an empty Event must never reach Deliver: GitHub has no verdict-less review.
func TestCommitDeliveryRefusesReviewWithNoVerdict(t *testing.T) {
	var called int32
	cfg := Config{
		Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
			called++
			return nil, nil
		},
	}
	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }

	commitDelivery(context.Background(), sink, cfg, "n1", workerActivity{
		stagedDelivery:     map[string]StagedDelivery{"review": {Kind: "review", Body: "some findings"}},
		skipArtifactRender: true,
	}, GateResult{Passed: true})

	if called != 0 {
		t.Fatalf("Deliver was called %d times, want 0 - a verdict-less review must be refused", called)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one delivery_result", events)
	}
	data, ok := events[0].Data.(stream.DeliveryResultData)
	if !ok {
		t.Fatalf("event data = %T, want DeliveryResultData", events[0].Data)
	}
	if data.Outcome != stream.DeliveryOutcomeFailed || data.Kind != "review" || data.Error == "" {
		t.Fatalf("delivery_result = %+v, want a failed review outcome naming the missing verdict", data)
	}
}

// Refusal is per item: a verdict-less review is refused while a sibling "pr" item in the same
// delivery still reaches Deliver.
func TestCommitDeliveryRefusesVerdictlessReviewButDeliversSiblingItem(t *testing.T) {
	var got DeliveryContext
	cfg := Config{
		Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
			got = dc
			return []DeliveryItemOutcome{{Kind: "pull_request", URL: "https://example/pr/1"}}, nil
		},
	}
	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }

	commitDelivery(context.Background(), sink, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{
			"review": {Kind: "review", Body: "some findings"}, // no Event
			"pr":     {Kind: "pull_request", Title: "t", Body: "b"},
		},
		skipArtifactRender: true,
	}, GateResult{Passed: true})

	if len(got.Items) != 1 || got.Items[0].Kind != "pull_request" {
		t.Fatalf("Items reaching Deliver = %+v, want only the pull_request item", got.Items)
	}
	var sawReviewRefusal bool
	for _, ev := range events {
		data, ok := ev.Data.(stream.DeliveryResultData)
		if ok && data.Kind == "review" && data.Outcome == stream.DeliveryOutcomeFailed {
			sawReviewRefusal = true
		}
	}
	if !sawReviewRefusal {
		t.Fatalf("events = %+v, want a failed review delivery_result alongside the delivered pr", events)
	}
}

// A permitted item in the SAME staged set as a refused one still ships - the
// allowlist is enforced per-item, not all-or-nothing for the node.
func TestCommitDeliveryDeliversGrantedItemsAlongsideRefusedOnes(t *testing.T) {
	var delivered []string
	cfg := Config{
		AllowedDeliveryKinds: []string{"pull_request"}, // no review
		Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
			for _, it := range dc.Items {
				delivered = append(delivered, it.Kind)
			}
			return nil, nil
		},
	}
	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }

	commitDelivery(context.Background(), sink, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{
			"review":       {Kind: "review", Event: "approve"},
			"pull_request": {Kind: "pull_request", Title: "fix CI"},
		},
	}, GateResult{Passed: true})

	if len(delivered) != 1 || delivered[0] != "pull_request" {
		t.Fatalf("Deliver saw items %v, want exactly the granted pull_request item", delivered)
	}
	var sawRefusedReview bool
	for _, ev := range events {
		if data, ok := ev.Data.(stream.DeliveryResultData); ok && data.Kind == "review" && data.Outcome == stream.DeliveryOutcomeFailed {
			sawRefusedReview = true
		}
	}
	if !sawRefusedReview {
		t.Fatalf("events = %+v, want a failed delivery_result naming the refused review", events)
	}
}

// A nil Config.AllowedDeliveryKinds (no trigger governs this run) must never
// refuse anything - the enforcement is scoped to runs a trigger explicitly bounds.
func TestCommitDeliveryNilAllowedKindsDeliversEverything(t *testing.T) {
	var called int32
	cfg := Config{Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		called++
		return nil, nil
	}}
	commitDelivery(context.Background(), nil, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve"}},
	}, GateResult{Passed: true})
	if called != 1 {
		t.Fatalf("Deliver called %d times, want 1 - a nil allowlist must not refuse anything", called)
	}
}

// Empty (non-nil) AllowedDeliveryKinds denies everything: "granted nothing", unlike nil's
// "no trigger governs this run".
func TestCommitDeliveryEmptyAllowedKindsDeniesEverything(t *testing.T) {
	var called int32
	cfg := Config{
		AllowedDeliveryKinds: []string{},
		Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
			called++
			return nil, nil
		},
	}
	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }
	commitDelivery(context.Background(), sink, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve"}},
	}, GateResult{Passed: true})
	if called != 0 {
		t.Fatalf("Deliver called %d times, want 0 - an empty (non-nil) allowlist must deny everything", called)
	}
}
