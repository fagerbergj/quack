package vetting

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// The case most likely to regress: Delivery.Kind "pull_request" means two
// different things depending on whether the run is issue-scoped (open a NEW
// PR, gated on OpenPR) or PR-scoped (push a commit to the EXISTING PR, gated
// on PushCommitsToPR). A grant holding one without the other must refuse the
// wrong side and allow the right one.
func TestGrantAllows_PullRequestKind_IssueScopedVsPRScoped(t *testing.T) {
	cases := []struct {
		name  string
		grant Grant
		kind  string
		want  bool
	}{
		{"issue-scoped with open_pr allows", Grant{PRScoped: false, OpenPR: true}, "pull_request", true},
		{"issue-scoped without open_pr refuses", Grant{PRScoped: false, OpenPR: false}, "pull_request", false},
		{"issue-scoped with ONLY push_commits_to_pr still refuses - wrong grant for opening a PR",
			Grant{PRScoped: false, PushCommitsToPR: true}, "pull_request", false},
		{"PR-scoped with push_commits_to_pr allows", Grant{PRScoped: true, PushCommitsToPR: true}, "pull_request", true},
		{"PR-scoped without push_commits_to_pr refuses", Grant{PRScoped: true, PushCommitsToPR: false}, "pull_request", false},
		{"PR-scoped with ONLY open_pr still refuses - wrong grant for pushing to an existing PR",
			Grant{PRScoped: true, OpenPR: true}, "pull_request", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.grant
			ok, reason := g.allows(tc.kind)
			if ok != tc.want {
				t.Fatalf("allows(%q) = %v (%q), want %v", tc.kind, ok, reason, tc.want)
			}
		})
	}
}

func TestGrantAllows_ReviewAndComment(t *testing.T) {
	if ok, _ := (&Grant{PostReview: false}).allows("review"); ok {
		t.Fatal("review must be refused without post_review")
	}
	if ok, _ := (&Grant{PostReview: true}).allows("review"); !ok {
		t.Fatal("review must be allowed with post_review")
	}
	if ok, _ := (&Grant{PRScoped: false, JoinIssueConversation: false}).allows("comment"); ok {
		t.Fatal("an issue-scoped comment must be refused without join_issue_conversation")
	}
	if ok, _ := (&Grant{PRScoped: true, JoinPRConversation: false}).allows("comment"); ok {
		t.Fatal("a PR-scoped comment must be refused without join_pr_conversation")
	}
}

// A nil Grant means no GitHub trigger governs the run (a plain REST/MCP
// conversation) - delivery must stay unrestricted.
func TestGrantAllows_NilGrantPermitsEverything(t *testing.T) {
	var g *Grant
	for _, kind := range []string{"pull_request", "review", "comment"} {
		if ok, reason := g.allows(kind); !ok {
			t.Fatalf("nil grant refused %q (%q), want unrestricted", kind, reason)
		}
	}
}

// commitDelivery is the sole enforcement point (#662): a node that stages a
// review it was never granted must have it refused, loudly - the delivery
// function must never even see it, and the refusal must surface as a failed
// delivery_result.
func TestCommitDeliveryRefusesUngrantedReview(t *testing.T) {
	var called int32
	cfg := Config{
		Grant: &Grant{PRScoped: true}, // no PostReview
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

// A permitted item in the SAME staged set as a refused one still ships - the
// grant is enforced per-item, not all-or-nothing for the node.
func TestCommitDeliveryDeliversGrantedItemsAlongsideRefusedOnes(t *testing.T) {
	var delivered []string
	cfg := Config{
		Grant: &Grant{PRScoped: true, PushCommitsToPR: true}, // no PostReview
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

// A nil Config.Grant (no GitHub trigger governs this run) must never refuse
// anything - the enforcement is scoped to GitHub-dispatched runs only.
func TestCommitDeliveryNilGrantDeliversEverything(t *testing.T) {
	var called int32
	cfg := Config{Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		called++
		return nil, nil
	}}
	commitDelivery(context.Background(), nil, cfg, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve"}},
	}, GateResult{Passed: true})
	if called != 1 {
		t.Fatalf("Deliver called %d times, want 1 - a nil grant must not refuse anything", called)
	}
}
