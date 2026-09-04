package serve

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/orchestrator"
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

// TestUpdateChatOrigin_PreservesStoredSetup is the #1181 review's blocking
// finding: newExtUpdateChatOrigin (the state-transition webhook path - PR
// synchronize/close/merge) used to marshal the bare extsdk.ChatOrigin and
// overwrite the whole row, wiping the quackSetup field a dispatch had just
// stored - so any such webhook between a dispatch and a nudge reopened
// #1180. A dispatch with Origin+Setup, then an origin update, must still
// have the stored Setup afterward.
func TestUpdateChatOrigin_PreservesStoredSetup(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)
	updateOrigin := newExtUpdateChatOrigin("noop", st, nil, nil)

	const localID = "update-origin-1181"
	chatID := "ext:noop:" + localID
	req := extsdk.DispatchRequest{
		Chat: extsdk.ChatRef{LocalID: localID, Origin: &extsdk.ChatOrigin{Extension: "noop", Label: "o/r#7", Kind: "pull_request"}},
		Ask:  extsdk.Ask{Message: "review this"},
		Run:  extsdk.RunConfig{Setup: &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-7"}},
	}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	// A state-transition webhook (e.g. PR synchronize) fires next, with no
	// Setup of its own - only the SDK's own ChatOrigin shape.
	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "o/r#7", Kind: "pull_request", Badge: "synchronize"}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if !strings.Contains(c.Origin, "quackSetup") {
		t.Fatalf("Origin = %q, lost quackSetup across the origin update", c.Origin)
	}
	var rec extOriginRecord
	if err := json.Unmarshal([]byte(c.Origin), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Setup == nil || rec.Setup.ExistingHeadRef != "quack/pr-7" {
		t.Errorf("Setup = %+v, want the dispatch's original head ref preserved", rec.Setup)
	}
	if rec.ChatOrigin == nil || rec.ChatOrigin.Badge != "synchronize" {
		t.Errorf("ChatOrigin = %+v, want the update's own Badge applied", rec.ChatOrigin)
	}
}
