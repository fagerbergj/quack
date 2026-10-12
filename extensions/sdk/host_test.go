package sdk_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// TestHostChatUserArchiveChat pins the ChatUser/ArchiveChat signatures quack wires as closures.
func TestHostChatUserArchiveChat(t *testing.T) {
	var archived []string
	h := sdk.Host{
		ChatUser: func(chatID string) (string, bool) {
			if chatID == "known" {
				return "alice", true
			}
			return "", false
		},
		ArchiveChat: func(chatID string) error {
			archived = append(archived, chatID)
			return nil
		},
	}

	if user, ok := h.ChatUser("known"); !ok || user != "alice" {
		t.Errorf("ChatUser(known) = (%q, %v), want (alice, true)", user, ok)
	}
	if _, ok := h.ChatUser("missing"); ok {
		t.Errorf("ChatUser(missing) ok = true, want false")
	}

	if err := h.ArchiveChat("c1"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if len(archived) != 1 || archived[0] != "c1" {
		t.Errorf("archived = %v, want [c1]", archived)
	}
}

// TestHostClassifyDegradesGracefullyWhenNil pins that a nil Classify (no
// judge model configured) is a valid, expected state - callers must check
// before calling, not assume it's always wired.
func TestHostClassifyDegradesGracefullyWhenNil(t *testing.T) {
	var h sdk.Host
	if h.Classify != nil {
		t.Fatalf("zero-value Host.Classify = non-nil, want nil")
	}

	h.Classify = func(ctx context.Context, prompt string) (string, error) {
		if prompt == "" {
			return "", errors.New("empty prompt")
		}
		return "WORK", nil
	}
	answer, err := h.Classify(context.Background(), "please review this PR")
	if err != nil || answer != "WORK" {
		t.Errorf("Classify = (%q, %v), want (WORK, nil)", answer, err)
	}
}

// TestHostUpdateChatOriginDegradesGracefullyWhenNilAndSignalsUnknownChat pins
// the shape of the badge-refresh addition: nil is a valid, expected state
// (matching every other best-effort Host call), and ErrUnknownChat is the
// documented sentinel for a localID that never reached Dispatch.
func TestHostUpdateChatOriginDegradesGracefullyWhenNilAndSignalsUnknownChat(t *testing.T) {
	var h sdk.Host
	if h.UpdateChatOrigin != nil {
		t.Fatalf("zero-value Host.UpdateChatOrigin = non-nil, want nil")
	}

	var updated []string
	h.UpdateChatOrigin = func(localID string, origin sdk.ChatOrigin) error {
		if localID == "missing" {
			return sdk.ErrUnknownChat
		}
		updated = append(updated, localID+":"+origin.Badge)
		return nil
	}

	if err := h.UpdateChatOrigin("issue-9", sdk.ChatOrigin{Extension: "github", Badge: "closed"}); err != nil {
		t.Fatalf("UpdateChatOrigin: %v", err)
	}
	if len(updated) != 1 || updated[0] != "issue-9:closed" {
		t.Errorf("updated = %v, want [issue-9:closed]", updated)
	}

	if err := h.UpdateChatOrigin("missing", sdk.ChatOrigin{}); !errors.Is(err, sdk.ErrUnknownChat) {
		t.Errorf("UpdateChatOrigin(missing) = %v, want ErrUnknownChat", err)
	}
}

// TestRunConfigTimeoutZeroMeansUnbounded pins the field's zero-value
// meaning at the type level.
func TestRunConfigTimeoutZeroMeansUnbounded(t *testing.T) {
	var rc sdk.RunConfig
	if rc.Timeout != 0 {
		t.Errorf("zero-value RunConfig.Timeout = %v, want 0 (unbounded)", rc.Timeout)
	}
	rc.Timeout = 2 * time.Hour
	if rc.Timeout != 2*time.Hour {
		t.Errorf("RunConfig.Timeout = %v, want 2h", rc.Timeout)
	}
}

// TestSetupExistingHeadRefOverridesWorkBranch pins the field's documented
// meaning (checkout an existing branch, not create WorkBranch fresh) at the
// type level - the actual override logic lives in quack, not the SDK.
func TestSetupExistingHeadRefOverridesWorkBranch(t *testing.T) {
	s := sdk.Setup{Repo: "https://github.com/acme/widgets", BaseRef: "main", WorkBranch: "quack/issue-9", ExistingHeadRef: "fix-typo"}
	if s.ExistingHeadRef != "fix-typo" {
		t.Errorf("ExistingHeadRef = %q, want fix-typo", s.ExistingHeadRef)
	}
	if s.WorkBranch != "quack/issue-9" {
		t.Errorf("WorkBranch = %q, want quack/issue-9 (ExistingHeadRef overrides at consumption time, not storage time)", s.WorkBranch)
	}
}

// TestSubjectStateConstValues pins the wire values hosts match on - these
// are a domain fact (open/merged/closed), not display text, so they must
// never shift under a cosmetic Badge rename.
func TestSubjectStateConstValues(t *testing.T) {
	cases := map[sdk.SubjectState]string{
		sdk.SubjectOpen:   "open",
		sdk.SubjectMerged: "merged",
		sdk.SubjectClosed: "closed",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("SubjectState const = %q, want %q", got, want)
		}
	}
}

// TestRunStatusConstValues pins the wire values a RunObserver matches on -
// including RunCancelled, added so a user-cancelled run has its own status
// instead of collapsing into RunDone with a mid-thought partial answer.
func TestRunStatusConstValues(t *testing.T) {
	cases := map[sdk.RunStatus]string{
		sdk.RunDone:       "done",
		sdk.RunFailed:     "failed",
		sdk.RunNeedsInput: "needs_input",
		sdk.RunCancelled:  "cancelled",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("RunStatus const = %q, want %q", got, want)
		}
	}
}

// TestChatOriginCarriesStateIndependentOfBadge pins that State is a
// distinct field from Badge - a host can read the typed state without
// parsing the display string.
func TestChatOriginCarriesStateIndependentOfBadge(t *testing.T) {
	o := sdk.ChatOrigin{Badge: "merged", State: sdk.SubjectMerged}
	if o.State != sdk.SubjectMerged {
		t.Errorf("State = %q, want %q", o.State, sdk.SubjectMerged)
	}
	if o.Badge != "merged" {
		t.Errorf("Badge = %q, want merged", o.Badge)
	}

	var zero sdk.ChatOrigin
	if zero.State != "" {
		t.Errorf("zero-value ChatOrigin.State = %q, want \"\" (unknown/not-applicable)", zero.State)
	}
}

// TestHostInvalidateSetupDegradesGracefullyWhenNil pins that a nil
// InvalidateSetup (an extension running against a core that predates it) is
// a valid state callers must check for, not assume away.
func TestHostInvalidateSetupDegradesGracefullyWhenNil(t *testing.T) {
	var h sdk.Host
	if h.InvalidateSetup != nil {
		t.Fatalf("zero-value Host.InvalidateSetup = non-nil, want nil")
	}

	var invalidated []string
	h.InvalidateSetup = func(chatID string) error {
		if chatID == "" {
			return errors.New("missing chat id")
		}
		invalidated = append(invalidated, chatID)
		return nil
	}
	if err := h.InvalidateSetup("ext:github:github-acme-widgets-9"); err != nil {
		t.Fatalf("InvalidateSetup: %v", err)
	}
	if len(invalidated) != 1 || invalidated[0] != "ext:github:github-acme-widgets-9" {
		t.Errorf("invalidated = %v, want [ext:github:github-acme-widgets-9]", invalidated)
	}
}

// TestHostDecideNilIsNoDecision pins that a nil Decide is a valid state and
// that a wired one carries the request through and returns the host's verdict.
func TestHostDecideNilIsNoDecision(t *testing.T) {
	var h sdk.Host
	if h.Decide != nil {
		t.Fatalf("zero-value Host.Decide = non-nil, want nil")
	}

	h.Decide = func(ctx context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
		if req.Point == "" {
			return sdk.Decision{}, errors.New("empty point")
		}
		return sdk.Decision{Top: "true", TopP: 0.95, Outcome: "act", Act: req.Baseline == "false"}, nil
	}
	d, err := h.Decide(context.Background(), sdk.DecideRequest{Point: "triage", State: "s", Baseline: "false"})
	if err != nil || !d.Act || d.Top != "true" {
		t.Errorf("Decide = (%+v, %v), want Act with Top true", d, err)
	}
	if _, err := h.Decide(context.Background(), sdk.DecideRequest{}); err == nil {
		t.Errorf("Decide with empty point: err = nil, want error")
	}
}

type declaringExt struct{ sdk.Extension }

func (declaringExt) DecisionPoints() []sdk.DecisionPoint {
	return []sdk.DecisionPoint{{Name: "triage", Primary: "q", Questions: map[string]sdk.DecisionQuestion{"q": {Type: "noul"}}}}
}

// TestDecisionPointsIsOptional pins that a host finds the declarations by type assertion.
func TestDecisionPointsIsOptional(t *testing.T) {
	var ext sdk.Extension = declaringExt{}
	dp, ok := ext.(sdk.DecisionPoints)
	if !ok || dp.DecisionPoints()[0].Name != "triage" {
		t.Fatalf("DecisionPoints not found on a declaring extension")
	}
}
