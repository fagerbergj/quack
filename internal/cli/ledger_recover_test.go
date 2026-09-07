package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
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

// fakeDeliveryRecords is a Projections.DeliveryRecorded/RecordDelivery
// double: a set of "targetID@revision" keys already recorded, standing in
// for the real delivery_record artifact (#1144 P2).
type fakeDeliveryRecords struct {
	done        map[string]bool
	recordCalls int
}

func newFakeDeliveryRecords() *fakeDeliveryRecords {
	return &fakeDeliveryRecords{done: map[string]bool{}}
}

func (f *fakeDeliveryRecords) checker(_ context.Context, _, targetID string, revision int) (bool, error) {
	return f.done[deliveryIdempotencyKeyForTest(targetID, revision)], nil
}

func (f *fakeDeliveryRecords) recorder(_ context.Context, _, _, targetID string, revision int, _ string) error {
	f.recordCalls++
	f.done[deliveryIdempotencyKeyForTest(targetID, revision)] = true
	return nil
}

func deliveryIdempotencyKeyForTest(targetID string, revision int) string {
	return fmt.Sprintf("%s@%d", targetID, revision)
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
	ls := ledger.NewMemStore()
	appendDeliveryIntentWithContextForTest(t, ls, "chat5", "code_review:pr:5@1", "code_review:pr:5", 1, "https://github.com/x/y.git", 5)

	rec := &fakeRecoverer{found: true}
	if _, err := RunLedgerRecover(ctx, ls, "chat5", Projections{Delivery: rec}, false); err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if rec.lastDC.CloneURL != "https://github.com/x/y.git" || rec.lastDC.IssueNumber != 5 {
		t.Fatalf("recoverer saw DeliveryContext %+v, want CloneURL/IssueNumber from the intent payload", rec.lastDC)
	}
}

// #1093 case 13, "found" branch (#1144 P2: completion is a delivery_record
// write, not a delivery.done entry). RecoverDelivery reports found=true, so
// recover calls RecordDelivery and never Redo (the extension is never asked
// to post twice).
func TestRunLedgerRecover_FoundRecordsDeliveryWithoutRedo(t *testing.T) {
	ctx := context.Background()
	ls := ledger.NewMemStore()
	appendDeliveryIntentForTest(t, ls, "chat1", "code_review:pr:1@2", "code_review:pr:1", 2)

	rec := &fakeRecoverer{found: true, remoteURL: "https://github.com/x/y/pull/1#pullrequestreview-1"}
	fdr := newFakeDeliveryRecords()
	redoCalls := 0
	report, err := RunLedgerRecover(ctx, ls, "chat1", Projections{
		Delivery: rec, DeliveryRecorded: fdr.checker, RecordDelivery: fdr.recorder,
		Redo: func(context.Context, OrphanedDelivery) error {
			redoCalls++
			return nil
		},
	}, false)
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Confirmed) != 1 || len(report.Redone) != 0 || len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v, want 1 confirmed, 0 redone, 0 unresolved", report)
	}
	if redoCalls != 0 {
		t.Fatalf("redoFunc called %d times, want 0 (extension already had it)", redoCalls)
	}
	if fdr.recordCalls != 1 {
		t.Fatalf("RecordDelivery called %d times, want 1", fdr.recordCalls)
	}

	// Re-running recover must not re-find the now-resolved intent.
	report2, err := RunLedgerRecover(ctx, ls, "chat1", Projections{Delivery: rec, DeliveryRecorded: fdr.checker, RecordDelivery: fdr.recorder}, false)
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
	ls := ledger.NewMemStore()
	appendDeliveryIntentForTest(t, ls, "chat2", "code_review:pr:2@1", "code_review:pr:2", 1)

	rec := &fakeRecoverer{found: false}
	var redone []OrphanedDelivery
	report, err := RunLedgerRecover(ctx, ls, "chat2", Projections{Delivery: rec, Redo: func(_ context.Context, o OrphanedDelivery) error {
		redone = append(redone, o)
		return nil
	}}, false)
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
	ls := ledger.NewMemStore()
	appendDeliveryIntentForTest(t, ls, "chat3", "document:doc:1@1", "document:doc:1", 1)

	report, err := RunLedgerRecover(ctx, ls, "chat3", Projections{}, false)
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Unresolved) != 1 {
		t.Fatalf("Unresolved = %d, want 1", len(report.Unresolved))
	}
}

// A delivery.intent WITH a matching delivery_record is not orphaned at all
// (#1144 P2: DeliveryRecorded is the single "is this done" read).
func TestRunLedgerRecover_NoOrphanWhenRecordExists(t *testing.T) {
	ctx := context.Background()
	ls := ledger.NewMemStore()
	appendDeliveryIntentForTest(t, ls, "chat4", "code_review:pr:4@1", "code_review:pr:4", 1)
	fdr := newFakeDeliveryRecords()
	fdr.done[deliveryIdempotencyKeyForTest("code_review:pr:4", 1)] = true

	report, err := RunLedgerRecover(ctx, ls, "chat4", Projections{Delivery: &fakeRecoverer{}, DeliveryRecorded: fdr.checker}, false)
	if err != nil {
		t.Fatalf("RunLedgerRecover: %v", err)
	}
	if len(report.Confirmed)+len(report.Redone)+len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v, want nothing (not orphaned)", report)
	}
}

// TestRecover_CrashBetweenDeliveryIntentAndRecord is the kill -9 case for
// delivery (#1144 P2): a delivery.intent lands, the process dies before the
// delivery_record artifact write. Recover (real vetting.DeliveryProjections
// against a real store) asks the extension, finds the delivery already
// landed, and writes the completing delivery_record - no CLI involved. A
// second pass finds nothing left to do.
func TestRecover_CrashBetweenDeliveryIntentAndRecord(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, targetID = "chat-delivery-crash", "ledgertest_doc:target-1"

	// The delivery target itself already exists at revision 1.
	c := recordstore.New(artifacts, "quack", "local", chatID).WithLedger(ls)
	if _, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"v": "1"}, targetID[len("ledgertest_doc:"):], recordstore.Lineage{Author: "tester"}); err != nil || rev != 1 {
		t.Fatalf("SaveStructured target: rev %d, %v", rev, err)
	}

	// Crash: delivery.intent lands, the delivery_record write never happens.
	payload, _ := json.Marshal(deliveryIntentPayload{TargetID: targetID, Revision: 1, Key: "code_review:target-1@1"})
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, NodeID: "n1", Kind: ledger.KindDeliveryIntent, Key: "code_review:target-1@1", Payload: payload}); err != nil {
		t.Fatal(err)
	}

	checker, recorder := vetting.DeliveryProjections(artifacts, ls, st.SessionUserForChat)
	proj := Projections{DeliveryRecorded: checker, RecordDelivery: recorder, Delivery: &fakeRecoverer{found: true, remoteURL: "https://example/pr/1"}}

	sum, err := Recover(ctx, ls, []string{chatID}, proj, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unresolved != 0 || len(sum.Reports) != 1 || len(sum.Reports[0].Confirmed) != 1 {
		t.Fatalf("summary = %+v, want one confirmed delivery", sum)
	}
	if done, err := checker(ctx, chatID, targetID, 1); err != nil || !done {
		t.Fatalf("delivery_record after recovery: done=%v err=%v", done, err)
	}

	// Idempotent: a second pass finds nothing left orphaned.
	again, err := Recover(ctx, ls, []string{chatID}, proj, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Reports) != 0 {
		t.Fatalf("second pass = %+v, want nothing", again)
	}
}

// TestRecover_TwoDeliveriesOnOneSubjectBothSettled is the blocker regression
// from review: a subject delivered TWICE (normal for re-review rounds) has a
// delivery_record history of two revisions, [rev1, rev2]. A Latest-only
// DeliveryRecorded would see only rev2 and re-flag rev1's intent as orphaned
// forever. Both intents must be found settled, and a recovery pass must call
// neither the recoverer nor RecordDelivery - nothing new gets appended.
func TestRecover_TwoDeliveriesOnOneSubjectBothSettled(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, targetID = "chat-two-deliveries", "code_review:target-2"

	c := recordstore.New(artifacts, "quack", "local", chatID)
	for rev := 1; rev <= 2; rev++ {
		if err := vetting.SaveDeliveryRecord(ctx, c, "n1", vetting.DeliveryRecord{
			TargetID: targetID, DeliveredRevision: rev, At: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("SaveDeliveryRecord rev %d: %v", rev, err)
		}
	}
	for rev := 1; rev <= 2; rev++ {
		payload, _ := json.Marshal(deliveryIntentPayload{TargetID: targetID, Revision: rev, Key: fmt.Sprintf("%s@%d", targetID, rev)})
		key := fmt.Sprintf("%s@%d", targetID, rev)
		if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, NodeID: "n1", Kind: ledger.KindDeliveryIntent, Key: key, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	checker, _ := vetting.DeliveryProjections(artifacts, ls, st.SessionUserForChat)
	rec := &fakeRecoverer{found: true} // must never be consulted - both intents are already settled
	recordCalls := 0
	proj := Projections{
		DeliveryRecorded: checker,
		Delivery:         rec,
		RecordDelivery: func(context.Context, string, string, string, int, string) error {
			recordCalls++
			return nil
		},
	}

	sum, err := Recover(ctx, ls, []string{chatID}, proj, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unresolved != 0 || len(sum.Reports) != 0 {
		t.Fatalf("summary = %+v, want nothing orphaned (both revisions already settled)", sum)
	}
	if rec.calls != 0 {
		t.Fatalf("DeliveryRecoverer consulted %d times, want 0 - both intents were already settled", rec.calls)
	}
	if recordCalls != 0 {
		t.Fatalf("RecordDelivery called %d times, want 0 - nothing should be appended", recordCalls)
	}
	if versions, err := c.Versions(ctx, "delivery_record:target-2"); err != nil || len(versions) != 2 {
		t.Fatalf("delivery_record versions = %v, err=%v, want exactly the 2 seeded above", versions, err)
	}
}

// TestRecover_CrashBetweenIntentAndRow is the kill -9 case: the WAL holds an
// artifact.revision intent whose row write never happened. After "restart",
// Recover marks it aborted, so the fold's parent revision agrees with the
// store again and the next save lands on the revision the store will assign.
func TestRecover_CrashBetweenIntentAndRow(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID = "chat-crash"
	c := recordstore.New(artifacts, "quack", "local", chatID).WithLedger(ls)
	id, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"v": "1"}, "doc-1", recordstore.Lineage{Author: "tester"})
	if err != nil || rev != 1 {
		t.Fatalf("SaveStructured: rev %d, %v", rev, err)
	}
	// Crash: the intent for revision 2 lands, the process dies before the row.
	payload, _ := json.Marshal(map[string]any{"id": id, "revision": 2, "parent_revision": 1, "bytes_ref": id})
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindArtifactRevision, Key: id, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if last, _ := fold.LastRevision(ctx, ls, chatID, id); last != 2 {
		t.Fatalf("fold before recovery = %d, want the phantom 2", last)
	}

	proj := Projections{ArtifactRowExists: ArtifactRowChecker(st, artifacts)}
	dry, err := Recover(ctx, ls, nil, proj, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Unresolved != 1 || len(dry.Reports) != 1 || len(dry.Reports[0].Aborted) != 1 {
		t.Fatalf("dry-run summary = %+v, want one row-less revision reported", dry)
	}
	if last, _ := fold.LastRevision(ctx, ls, chatID, id); last != 2 {
		t.Fatalf("dry-run wrote: fold = %d", last)
	}

	sum, err := Recover(ctx, ls, nil, proj, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unresolved != 0 || len(sum.Reports) != 1 || sum.Reports[0].Aborted[0].Revision != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if last, _ := fold.LastRevision(ctx, ls, chatID, id); last != 1 {
		t.Fatalf("fold after recovery = %d, want the store's real revision 1", last)
	}
	// Idempotent: a second pass finds nothing.
	again, _ := Recover(ctx, ls, nil, proj, false)
	if len(again.Reports) != 0 {
		t.Fatalf("second pass = %+v, want nothing", again)
	}
	// And the next save lands where the store assigns it.
	if _, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"v": "2"}, "doc-1", recordstore.Lineage{Author: "tester", ParentRevision: 1}); err != nil || rev != 2 {
		t.Fatalf("save after recovery: rev %d, %v", rev, err)
	}
}
