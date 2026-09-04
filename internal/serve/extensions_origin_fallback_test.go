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
	storedJSON, effective := mergeExtOrigin("", origin, first)
	if effective == nil || effective.WorkBranch != "quack/pr-1170" || !effective.CheckoutExistingHead {
		t.Fatalf("first dispatch's own Setup should be the effective one, got %+v", effective)
	}
	if storedJSON == "" {
		t.Fatal("first dispatch stored nothing")
	}

	// Turn 2: the nudge - no Chat.Origin, no Run.Setup.
	nudgedJSON, effective := mergeExtOrigin(storedJSON, nil, nil)
	if effective == nil {
		t.Fatal("nudge got no effective setup - the exact #1180/quack-extensions#47 gap")
	}
	if effective.WorkBranch != "quack/pr-1170" || !effective.CheckoutExistingHead {
		t.Errorf("effective = %+v, want the stored PR head checked out as-is", effective)
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
// stored (a plain, non-PR dispatch) gets no effective setup - plan.go's
// existing "no setup anywhere" rejection is still what a bare nudge on such a
// chat sees.
func TestMergeExtOrigin_NoPriorSetup_NoFallback(t *testing.T) {
	_, effective := mergeExtOrigin("", nil, nil)
	if effective != nil {
		t.Errorf("effective = %+v, want nil with nothing ever stored", effective)
	}
}

// TestMergeExtOrigin_FreshSetupOverridesStale: a dispatch that carries its
// own Setup WITH a real head ref must use that, not a stale one from an
// earlier, unrelated turn.
func TestMergeExtOrigin_FreshSetupOverridesStale(t *testing.T) {
	stale := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-old"}
	storedJSON, _ := mergeExtOrigin("", nil, stale)

	fresh := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-new"}
	_, effective := mergeExtOrigin(storedJSON, nil, fresh)
	if effective == nil || effective.WorkBranch != "quack/pr-new" {
		t.Fatalf("effective = %+v, want this dispatch's own fresh head ref", effective)
	}
}

// TestMergeExtOrigin_BlankHeadRefBorrowsStored is #1180's actual recurrence
// (issue comment: "neither the v0.9.0 nudge re-send ... nor #1181's
// stored-origin fallback delivered a head ref"): github's own dispatch()
// ALWAYS builds a non-nil sdk.Setup, even when its snapshot fetch came back
// without a head ref - so "newSetup == nil" (the only case #1181 handled)
// never actually happens on a real github dispatch. A Setup that is present
// but blank must get the same fallback as a wholly-missing one.
func TestMergeExtOrigin_BlankHeadRefBorrowsStored(t *testing.T) {
	good := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", ExistingHeadRef: "quack/pr-1188"}
	storedJSON, _ := mergeExtOrigin("", nil, good)

	// This dispatch (or its nudge) built a real, non-nil Setup, but this
	// time the head ref came back empty.
	blank := &extsdk.Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/issue-1188"}
	_, effective := mergeExtOrigin(storedJSON, nil, blank)
	if effective == nil || effective.WorkBranch != "quack/pr-1188" || !effective.CheckoutExistingHead {
		t.Fatalf("effective = %+v, want the stored real head ref borrowed onto this dispatch's Setup", effective)
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
