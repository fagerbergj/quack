package vetting

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

func seedCodeReview(t *testing.T, cfg Config, verdict, summary string, findings map[string]FindingRecord) {
	t.Helper()
	c := recordClient(cfg)
	if c == nil {
		t.Fatal("recordClient nil - Config.Artifacts must be set")
	}
	ids := make([]string, 0, len(findings))
	for id, f := range findings {
		if _, _, err := c.SaveStructured(context.Background(), kindFinding, f, "", recordstore.Lineage{}); err != nil {
			t.Fatalf("seed finding %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	rec := CodeReviewRecord{Verdict: verdict, Summary: summary, FindingIDs: ids}
	if _, _, err := c.SaveStructured(context.Background(), kindCodeReview, rec, SubjectHint(cfg.ChatID), recordstore.Lineage{}); err != nil {
		t.Fatalf("seed code_review: %v", err)
	}
}

// #1093: a reviewer node with a passed round's code_review/finding records
// delivers from them, not from the worker's own staged text.
func TestCommitDelivery_RendersReviewFromArtifact(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-42", User: "u1", Artifacts: artifact.InMemoryService()}
	finding := FindingRecord{Path: "a.go", LineHint: 10, Title: "unchecked error", Rationale: "err is dropped", State: "new"}
	fid, err := recordstore.IdentityFor(kindFinding, finding, "")
	if err != nil {
		t.Fatalf("finding identity: %v", err)
	}
	seedCodeReview(t, cfg, "request_changes", "artifact summary", map[string]FindingRecord{fid: finding})

	var got DeliveryContext
	cfg.Deliver = func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		got = dc
		return []DeliveryItemOutcome{{Kind: "review", URL: "https://example/review/1"}}, nil
	}
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "comment", Body: "STALE staged text"}}}
	commitDelivery(context.Background(), func(stream.SSEEvent) {}, cfg, "n1", act, GateResult{Passed: true})

	if len(got.Items) != 1 {
		t.Fatalf("Items = %+v, want exactly the rendered review", got.Items)
	}
	item := got.Items[0]
	if strings.Contains(item.Body, "STALE staged text") {
		t.Fatalf("Body = %q, want the artifact render, not the stale staged text", item.Body)
	}
	if !strings.Contains(item.Body, "artifact summary") {
		t.Fatalf("Body = %q, want the code_review record's summary", item.Body)
	}
	if item.Event != "request_changes" {
		t.Fatalf("Event = %q, want the record's verdict", item.Event)
	}
	if len(item.Comments) != 1 || !strings.Contains(item.Comments[0].Body, "unchecked error") {
		t.Fatalf("Comments = %+v, want the finding rendered inline", item.Comments)
	}
}

// Fallback: no code_review artifact exists yet for this subject -> today's
// staged-text delivery, unchanged.
func TestCommitDelivery_FallsBackToStagedTextWithoutArtifact(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-43", User: "u1", Artifacts: artifact.InMemoryService()}
	var got DeliveryContext
	cfg.Deliver = func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		got = dc
		return nil, nil
	}
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "worker's own text"}}}
	commitDelivery(context.Background(), func(stream.SSEEvent) {}, cfg, "n1", act, GateResult{Passed: true})

	if len(got.Items) != 1 || got.Items[0].Body != "worker's own text" {
		t.Fatalf("Items = %+v, want the staged item delivered as-is", got.Items)
	}
}

// #1093 case 8: a second delivered revision renders unchanged findings as a
// carried-over reference, not a duplicate full comment, and adds a second
// delivery_record entry.
func TestCommitDelivery_SecondRevisionCarriesOverUnchangedFindings(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-44", User: "u1", Artifacts: artifact.InMemoryService()}
	finding := FindingRecord{Path: "a.go", LineHint: 10, Title: "unchecked error", Rationale: "err is dropped", State: "new"}
	fid, err := recordstore.IdentityFor(kindFinding, finding, "")
	if err != nil {
		t.Fatalf("finding identity: %v", err)
	}
	seedCodeReview(t, cfg, "request_changes", "round 1", map[string]FindingRecord{fid: finding})

	var calls []DeliveryContext
	cfg.Deliver = func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		calls = append(calls, dc)
		return []DeliveryItemOutcome{{Kind: "review", URL: "https://example/review/1"}}, nil
	}
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "irrelevant"}}}
	commitDelivery(context.Background(), func(stream.SSEEvent) {}, cfg, "n1", act, GateResult{Passed: true})
	if len(calls) != 1 {
		t.Fatalf("first delivery: Deliver called %d times, want 1", len(calls))
	}

	// Round 2: the same finding reappears "unchanged"; code_review gets a new revision.
	unchanged := finding
	unchanged.State = "unchanged"
	seedCodeReview(t, cfg, "request_changes", "round 2", map[string]FindingRecord{fid: unchanged})
	act2 := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "irrelevant"}}}
	commitDelivery(context.Background(), func(stream.SSEEvent) {}, cfg, "n1", act2, GateResult{Passed: true})
	if len(calls) != 2 {
		t.Fatalf("second delivery: Deliver called %d times total, want 2 (posts again, not an edit)", len(calls))
	}

	second := calls[1].Items[0]
	if strings.Contains(second.Comments[0].Body, "err is dropped") == false || !strings.Contains(second.Comments[0].Body, "carried over") {
		t.Fatalf("second delivery comment = %+v, want a carried-over reference, not a fresh duplicate", second.Comments)
	}

	id, _ := recordstore.IdentityFor(kindCodeReview, nil, SubjectHint(cfg.ChatID))
	entries := listDeliveryRecords(context.Background(), cfg, id)
	if len(entries) != 2 {
		t.Fatalf("delivery_record entries = %d, want 2 (one per delivered revision)", len(entries))
	}
}

// delivery.intent append failure is fail-closed: Deliver must never be
// called, and the outcome is reported failed.
func TestCommitDelivery_IntentAppendFailureBlocksDelivery(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-45", User: "u1", Artifacts: artifact.InMemoryService()}
	finding := FindingRecord{Path: "a.go", Title: "x", State: "new"}
	fid, _ := recordstore.IdentityFor(kindFinding, finding, "")
	seedCodeReview(t, cfg, "approve", "s", map[string]FindingRecord{fid: finding})

	fl := newFakeGateLedger()
	fl.failKind = ledger.KindDeliveryIntent
	fl.failOccurrence = 1
	cfg.Ledger = fl

	var delivered bool
	cfg.Deliver = func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		delivered = true
		return nil, nil
	}
	var outcomes []stream.SSEEvent
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "x"}}}
	commitDelivery(context.Background(), func(ev stream.SSEEvent) { outcomes = append(outcomes, ev) }, cfg, "n1", act, GateResult{Passed: true})

	if delivered {
		t.Fatal("Deliver was called despite a failed delivery.intent WAL append")
	}
	if len(outcomes) == 0 {
		t.Fatal("no delivery outcome event emitted for the blocked delivery")
	}
}
