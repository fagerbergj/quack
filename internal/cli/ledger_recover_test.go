package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

type fakeRecoverer struct {
	found     bool
	remoteURL string
	calls     int
	lastDC    DeliveryContext
}

func (f *fakeRecoverer) RecoverDelivery(_ context.Context, _ string, dc DeliveryContext) (bool, DeliveryItemOutcome, error) {
	f.calls++
	f.lastDC = dc
	return f.found, DeliveryItemOutcome{URL: f.remoteURL}, nil
}

func appendDeliveryIntentForTest(t *testing.T, ls ledger.LedgerStore, chatID, key, targetID string, revision int) {
	t.Helper()
	appendDeliveryIntentWithContextForTest(t, ls, chatID, key, targetID, revision, "", 0)
}

func appendDeliveryIntentWithContextForTest(t *testing.T, ls ledger.LedgerStore, chatID, key, targetID string, revision int, cloneURL string, issueNumber int) {
	t.Helper()
	payload, _ := json.Marshal(deliveryIntentPayload{TargetID: targetID, Revision: revision, Key: key, CloneURL: cloneURL, IssueNumber: issueNumber})
	if _, err := ls.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, NodeID: "n1", Kind: ledger.KindDeliveryIntent, Key: key, Payload: payload,
	}); err != nil {
		t.Fatalf("append delivery.intent: %v", err)
	}
}

// #1093 finding 4: the recoverer must receive a DeliveryContext rebuilt from
// the persisted intent payload, not a zero value - offline recovery has no
// live worker activity to derive clone/PR coordinates from.
func TestRunLedgerRecover_RebuildsDeliveryContextFromIntent(t *testing.T) {
	ctx := context.Background()
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	appendDeliveryIntentWithContextForTest(t, ls, "chat5", "code_review:pr:5@1", "code_review:pr:5", 1, "https://github.com/x/y.git", 5)

	rec := &fakeRecoverer{found: true}
	if _, err := RunLedgerRecover(ctx, ls, "chat5", rec, nil); err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if rec.lastDC.CloneURL != "https://github.com/x/y.git" || rec.lastDC.IssueNumber != 5 {
		t.Fatalf("recoverer saw DeliveryContext %+v, want CloneURL/IssueNumber from the intent payload", rec.lastDC)
	}
}

// #1093 case 13, "found" branch: a crash between Deliver succeeding and
// delivery.done landing. RecoverDelivery reports found=true, so recover
// appends delivery.done and never calls redoFunc (the extension is never
// asked to post twice).
func TestRunLedgerRecover_FoundAppendsDoneWithoutRedo(t *testing.T) {
	ctx := context.Background()
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	appendDeliveryIntentForTest(t, ls, "chat1", "code_review:pr:1@2", "code_review:pr:1", 2)

	rec := &fakeRecoverer{found: true, remoteURL: "https://github.com/x/y/pull/1#pullrequestreview-1"}
	redoCalls := 0
	report, err := RunLedgerRecover(ctx, ls, "chat1", rec, func(context.Context, OrphanedDelivery) error {
		redoCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Confirmed) != 1 || len(report.Redone) != 0 || len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v, want 1 confirmed, 0 redone, 0 unresolved", report)
	}
	if redoCalls != 0 {
		t.Fatalf("redoFunc called %d times, want 0 (extension already had it)", redoCalls)
	}

	entries, err := ls.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	doneCount := 0
	for _, e := range entries {
		if e.Kind == ledger.KindDeliveryDone && e.Key == "code_review:pr:1@2" {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Fatalf("delivery.done entries for key = %d, want 1", doneCount)
	}

	// Re-running recover must not re-find the now-resolved intent.
	report2, err := RunLedgerRecover(ctx, ls, "chat1", rec, nil)
	if err != nil {
		t.Fatalf("RunLedgerRecover (2nd): %v", err)
	}
	if len(report2.Confirmed) != 0 {
		t.Fatalf("second recover found %d confirmed, want 0 (already reconciled)", len(report2.Confirmed))
	}
}

// #1093 case 13, "not found" branch: a crash BEFORE Deliver ever reached the
// extension. RecoverDelivery reports found=false, so recover calls redoFunc
// to redo the delivery the same way it would have run the first time.
func TestRunLedgerRecover_NotFoundRedoes(t *testing.T) {
	ctx := context.Background()
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	appendDeliveryIntentForTest(t, ls, "chat2", "code_review:pr:2@1", "code_review:pr:2", 1)

	rec := &fakeRecoverer{found: false}
	var redone []OrphanedDelivery
	report, err := RunLedgerRecover(ctx, ls, "chat2", rec, func(_ context.Context, o OrphanedDelivery) error {
		redone = append(redone, o)
		return nil
	})
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Redone) != 1 || len(report.Confirmed) != 0 {
		t.Fatalf("report = %+v, want 1 redone, 0 confirmed", report)
	}
	if len(redone) != 1 || redone[0].TargetID != "code_review:pr:2" || redone[0].Revision != 1 {
		t.Fatalf("redoFunc saw %+v", redone)
	}
}

// No recoverer wired (today's cmd/quack wiring): every orphan is reported
// Unresolved rather than guessed at.
func TestRunLedgerRecover_NoRecovererReportsUnresolved(t *testing.T) {
	ctx := context.Background()
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	appendDeliveryIntentForTest(t, ls, "chat3", "document:doc:1@1", "document:doc:1", 1)

	report, err := RunLedgerRecover(ctx, ls, "chat3", nil, nil)
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Unresolved) != 1 {
		t.Fatalf("Unresolved = %d, want 1", len(report.Unresolved))
	}
}

// A delivery.intent WITH a matching delivery.done is not orphaned at all.
func TestRunLedgerRecover_NoOrphanWhenDoneExists(t *testing.T) {
	ctx := context.Background()
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	appendDeliveryIntentForTest(t, ls, "chat4", "code_review:pr:4@1", "code_review:pr:4", 1)
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: "chat4", Kind: ledger.KindDeliveryDone, Key: "code_review:pr:4@1"}); err != nil {
		t.Fatalf("append delivery.done: %v", err)
	}

	report, err := RunLedgerRecover(ctx, ls, "chat4", &fakeRecoverer{}, nil)
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Confirmed)+len(report.Redone)+len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v, want nothing (not orphaned)", report)
	}
}
