package serve

import (
	"encoding/json"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
)

// TestMergeExtOrigin_NudgeFallsBackToStoredSetup is #1180's issue-47
// companion: a nudge/retry re-dispatch on an already-dispatched PR chat
// carries no Run.Setup - mergeExtOrigin must hand back the head ref a prior
// dispatch on the same chat stored, instead of silently blanking it.
func TestMergeExtOrigin_NudgeFallsBackToStoredSetup(t *testing.T) {
	first := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-1170"}
	origin := &extsdk.ChatOrigin{Extension: "github", Label: "o/r#1170", Kind: "pr"}
	storedJSON, fallback := mergeExtOrigin("", origin, first)
	if fallback != nil {
		t.Fatalf("first dispatch carried its own Setup; want no fallback, got %+v", fallback)
	}
	if storedJSON == "" {
		t.Fatal("first dispatch stored nothing")
	}

	// Turn 2: the nudge - no Chat.Origin, no Run.Setup.
	nudgedJSON, fallback := mergeExtOrigin(storedJSON, nil, nil)
	if fallback == nil {
		t.Fatal("nudge got no fallback setup - the exact #1180/quack-extensions#47 gap")
	}
	if fallback.WorkBranch != "quack/pr-1170" || !fallback.CheckoutExistingHead {
		t.Errorf("fallback = %+v, want the stored PR head checked out as-is", fallback)
	}
	// The merged record must still carry the origin - a nudge with no Origin
	// of its own must not blank the chat's sidebar/title metadata either.
	var rec extOriginRecord
	if err := json.Unmarshal([]byte(nudgedJSON), &rec); err != nil {
		t.Fatalf("unmarshal merged origin: %v", err)
	}
	if rec.ChatOrigin == nil || rec.ChatOrigin.Label != "o/r#1170" {
		t.Errorf("merged origin lost the prior ChatOrigin: %+v", rec.ChatOrigin)
	}
	if rec.Setup == nil || rec.Setup.ExistingHeadRef != "quack/pr-1170" {
		t.Errorf("merged record lost the prior Setup: %+v", rec.Setup)
	}
}

// TestMergeExtOrigin_NoPriorSetup_NoFallback: a chat that never had a Setup
// stored (a plain, non-PR dispatch) gets no fallback - plan.go's existing
// "no setup anywhere" rejection is still what a bare nudge on such a chat sees.
func TestMergeExtOrigin_NoPriorSetup_NoFallback(t *testing.T) {
	_, fallback := mergeExtOrigin("", nil, nil)
	if fallback != nil {
		t.Errorf("fallback = %+v, want nil with nothing ever stored", fallback)
	}
}

// TestMergeExtOrigin_FreshSetupOverridesStale: a dispatch that DOES carry its
// own Setup must use that, not a stale one from an earlier, unrelated turn.
func TestMergeExtOrigin_FreshSetupOverridesStale(t *testing.T) {
	stale := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-old"}
	storedJSON, _ := mergeExtOrigin("", nil, stale)

	fresh := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-new"}
	_, fallback := mergeExtOrigin(storedJSON, nil, fresh)
	if fallback != nil {
		t.Fatalf("this dispatch carried its own Setup; want no fallback, got %+v", fallback)
	}
}
