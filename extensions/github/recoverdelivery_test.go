package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// RecoverDelivery finds a prior review carrying the idempotency key, and reports not-found
// when none carries it (a fresh key, or a review with no key marker).
func TestRecoverDelivery(t *testing.T) {
	reviews := []prReview{
		{ID: 1, HTMLURL: "https://github.com/acme/widgets/pull/7#pullrequestreview-1", Body: "looks fine" + deliveryKeyMarker("code_review:pr:7@1")},
		{ID: 2, HTMLURL: "https://github.com/acme/widgets/pull/7#pullrequestreview-2", Body: "no marker at all"},
	}
	app := seededApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/7/reviews" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(reviews)
	})
	dc := sdk.DeliveryContext{CloneURL: "https://github.com/acme/widgets", IssueNumber: 7}

	found, outcome, err := app.RecoverDelivery(context.Background(), "code_review:pr:7@1", dc)
	if err != nil {
		t.Fatalf("RecoverDelivery: %v", err)
	}
	if !found || outcome.URL != reviews[0].HTMLURL {
		t.Fatalf("found=%v outcome=%+v, want the review carrying that key", found, outcome)
	}

	found2, _, err := app.RecoverDelivery(context.Background(), "code_review:pr:7@2", dc)
	if err != nil {
		t.Fatalf("RecoverDelivery: %v", err)
	}
	if found2 {
		t.Fatal("found=true for a key no review carries")
	}
}

func TestDeliveryKeyMarkerEmptyKeyEmbedsNothing(t *testing.T) {
	if got := deliveryKeyMarker(""); got != "" {
		t.Fatalf("deliveryKeyMarker(\"\") = %q, want empty (pre-#1093 delivery, nothing to embed)", got)
	}
}
