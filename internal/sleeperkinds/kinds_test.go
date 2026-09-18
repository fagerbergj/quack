package sleeperkinds

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestKindsRegistered checks every sleeper artifact kind lands in the
// closed set agent-card.json's "artifact" field may select from.
func TestKindsRegistered(t *testing.T) {
	for _, kind := range kindNames {
		if err := recordstore.ValidateArtifactKind(kind); err != nil {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
}

// TestIdentityMatchesHintedSaveBlob proves vetting.deliveryTarget's lookup id
// agrees with the id a hinted SaveBlob write actually lands on.
func TestIdentityMatchesHintedSaveBlob(t *testing.T) {
	for _, kind := range kindNames {
		t.Run(kind, func(t *testing.T) {
			const hint = "chat:test-chat"
			wantID, err := recordstore.IdentityFor(kind, nil, hint)
			if err != nil {
				t.Fatalf("IdentityFor: %v", err)
			}
			c := recordstore.New(artifact.InMemoryService(), "quack", "u", "s")
			gotID, _, err := c.SaveBlob(context.Background(), kind, []byte("body"), "text/plain", hint, recordstore.Lineage{})
			if err != nil {
				t.Fatalf("SaveBlob: %v", err)
			}
			if gotID != wantID {
				t.Errorf("SaveBlob id %q != IdentityFor id %q", gotID, wantID)
			}
		})
	}
}

// TestIdentityFromHintRequiresHint covers the error branch: RequiresHint
// means callers never pass an empty hint, but Identity must still refuse one.
func TestIdentityFromHintRequiresHint(t *testing.T) {
	if _, err := identityFromHint([]byte("body"), ""); err == nil {
		t.Error("want error for empty hint, got nil")
	}
}
