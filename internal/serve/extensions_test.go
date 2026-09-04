package serve

import (
	"context"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/vetting"
)

// fakeSDKRecoverer stands in for an extension's sdk.DeliveryRecoverer.
type fakeSDKRecoverer struct{ gotDC extsdk.DeliveryContext }

func (f *fakeSDKRecoverer) RecoverDelivery(_ context.Context, key string, dc extsdk.DeliveryContext) (bool, extsdk.DeliveryItemOutcome, error) {
	f.gotDC = dc
	return true, extsdk.DeliveryItemOutcome{Kind: "review", URL: "https://example/pr/1#review-1"}, nil
}

func TestSdkRecoverAdapterForwardsAndMapsOutcome(t *testing.T) {
	fake := &fakeSDKRecoverer{}
	adapter := sdkRecoverAdapter{recoverer: fake}

	found, outcome, err := adapter.RecoverDelivery(context.Background(), "key1", cli.DeliveryContext{
		CloneURL: "https://github.com/x/y.git", IssueNumber: 7,
	})
	if err != nil {
		t.Fatalf("RecoverDelivery: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if fake.gotDC.CloneURL != "https://github.com/x/y.git" || fake.gotDC.IssueNumber != 7 {
		t.Errorf("sdk saw DeliveryContext %+v, want CloneURL/IssueNumber forwarded", fake.gotDC)
	}
	if outcome.URL != "https://example/pr/1#review-1" || outcome.Kind != "review" {
		t.Errorf("outcome = %+v, want it mapped from the sdk outcome", outcome)
	}
}

// fakeDeliverer captures the sdk.DeliveryContext it receives so the test can
// assert sdkDeliverAdapter forwarded the fields quack's own DeliveryContext
// set (#1158 PushError, #1093 IdempotencyKey).
type fakeDeliverer struct{ got extsdk.DeliveryContext }

func (f *fakeDeliverer) Deliver(ctx context.Context, dc extsdk.DeliveryContext) ([]extsdk.DeliveryItemOutcome, error) {
	f.got = dc
	return nil, nil
}

func TestSdkDeliverAdapterForwardsPushErrorAndIdempotencyKey(t *testing.T) {
	fake := &fakeDeliverer{}
	adapter := sdkDeliverAdapter{deliverer: fake}

	_, err := adapter.Deliver(context.Background(), vetting.DeliveryContext{
		PushError:      "push rejected: non-fast-forward",
		IdempotencyKey: "artifact-42@3",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if fake.got.PushError != "push rejected: non-fast-forward" {
		t.Errorf("PushError = %q, want it forwarded from quack's DeliveryContext", fake.got.PushError)
	}
	if fake.got.IdempotencyKey != "artifact-42@3" {
		t.Errorf("IdempotencyKey = %q, want it forwarded from quack's DeliveryContext", fake.got.IdempotencyKey)
	}
}
