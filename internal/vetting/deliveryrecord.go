// deliveryrecord.go: the "delivery_record" kind (#1093, P6/P10 of the
// artifact-model epic #1090) - one id per subject (delivery_record:<subject>),
// one revision per delivery, appended via the normal SaveStructured
// revision-append mechanism (recordstore's save() always appends the next
// revision under a lock - the same one code_review uses).
package vetting

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
)

const kindDeliveryRecord = "delivery_record"

func init() {
	recordstore.Register(kindDeliveryRecord, recordstore.KindSpec{
		Class: recordstore.Structured,
		// Schema-less: gate-written only, no worker ever calls
		// write_delivery_record (mirrors kindJudgeRound's rationale).
		JSONSchema: `{"type":"object"}`,
		Validate:   validateJSONObject[DeliveryRecord],
		// Instance = hint verbatim, the subject (e.g. "pr:123") - one id per
		// subject, ALL delivered revisions of it living as that id's own
		// revision history (§4.9's history read = list this one id's
		// revisions, not scan-by-prefix across many ids).
		Identity:     func(_ []byte, hint string) (string, error) { return requireHint(hint) },
		RequiresHint: true,
	})
}

// DeliveryRecord: the "delivery_record" kind's body (#1090 §4.3/§9, minimal
// shape). RemoteURL alone (not a decomposed review_id/comment_id/pr_number)
// is what every extension's DeliveryItemOutcome already reports today;
// PRNumber is derived from dc.IssueNumber, which core already has.
type DeliveryRecord struct {
	TargetID          string    `json:"target_id"`
	DeliveredRevision int       `json:"delivered_revision"`
	RemoteURL         string    `json:"remote_url,omitempty"`
	PRNumber          int       `json:"pr_number,omitempty"`
	At                time.Time `json:"at"`
	// GatePassed: false on a gate-fail-but-still-delivers round (draft PR
	// per design V4 §4.5) - DeliveredRevision is still the artifact revision
	// that was actually rendered and posted, never a staged-text fallback.
	GatePassed bool `json:"gate_passed"`
	// RenderedFromStaged: true when no artifact-backed render existed and
	// the worker's own staged text was posted instead (finding 2) - such a
	// delivery must never be confused with an artifact-backed one when
	// diffing carried-over/resolved findings later.
	RenderedFromStaged bool `json:"rendered_from_staged,omitempty"`
	// Error: non-empty on a failed delivery attempt (e.g. gate push failure,
	// #1155) - kept in the history so a later revision isn't mistaken for
	// the subject's first attempt.
	Error string `json:"error,omitempty"`
}

// deliverySubject strips the leading "<kind>:" off a target id ("code_review:
// pr:123" -> "pr:123") - the delivery_record id is one per subject, not one
// per (subject, kind of the thing that triggered it).
func deliverySubject(targetID string) string {
	if _, subject, ok := strings.Cut(targetID, ":"); ok {
		return subject
	}
	return targetID
}

// deliveryRecordID composes the one delivery_record id for a subject.
func deliveryRecordID(targetID string) string {
	return kindDeliveryRecord + ":" + deliverySubject(targetID)
}

// saveDeliveryRecord appends one delivery_record revision (#1093). Fail-open:
// a save error is Warned, matching every other episodic write in this
// package - the delivery itself already happened by the time this is called.
func saveDeliveryRecord(ctx context.Context, cfg Config, nodeID string, rec DeliveryRecord) {
	if err := SaveDeliveryRecord(ctx, recordClient(cfg), nodeID, rec); err != nil {
		slog.Warn("delivery_record save failed", "component", "vetting", "node", nodeID, "target", rec.TargetID, "err", err)
	}
}

// SaveDeliveryRecord writes rec as its subject's next delivery_record
// revision - the WAL's completion for a delivery.intent (#1144 P2: one
// representation per fact, no separate delivery.done entry). Exported for
// `quack ledger recover`/boot recovery, which reconstruct a *recordstore.Client
// directly rather than a full vetting.Config. nil client is a no-op.
func SaveDeliveryRecord(ctx context.Context, c *recordstore.Client, nodeID string, rec DeliveryRecord) error {
	if c == nil {
		return nil
	}
	lineage := recordstore.Lineage{NodeID: nodeID, SavedAt: rec.At}
	_, _, err := c.SaveStructured(ctx, kindDeliveryRecord, rec, deliverySubject(rec.TargetID), lineage)
	return err
}

// DeliveryRecorded reports whether ANY revision in targetID's delivery_record
// history is a successful (no Error) record of revision - the recovery read
// that replaces the deleted delivery.done ledger entry. Must walk the whole
// history, not just Latest: the record is one id per SUBJECT with one
// revision per delivery (listDeliveryRecords's doc), so a subject delivered
// more than once has an older settled intent whose revision is no longer the
// latest one - a Latest-only check would re-flag it as unsettled forever.
func DeliveryRecorded(ctx context.Context, c *recordstore.Client, targetID string, revision int) (bool, error) {
	if c == nil {
		return false, nil
	}
	id := deliveryRecordID(targetID)
	versions, err := c.Versions(ctx, id)
	if err != nil {
		return false, err
	}
	for _, v := range versions {
		raw, ok, err := c.LoadVersion(ctx, id, v)
		if err != nil || !ok {
			continue
		}
		var rec DeliveryRecord
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if rec.DeliveredRevision == revision && rec.Error == "" {
			return true, nil
		}
	}
	return false, nil
}

// DeliveryProjections builds the checker/recorder pair boot recovery and
// `quack ledger recover` need to read and write delivery_record completions
// (#1144 P2) without a live vetting.Config - userFor resolves the chat's
// owning user the same way ArtifactRowChecker does.
func DeliveryProjections(artifacts artifact.Service, ledgerStore ledger.LedgerStore, userFor func(ctx context.Context, chatID string) string) (
	checker func(ctx context.Context, chatID, targetID string, revision int) (bool, error),
	recorder func(ctx context.Context, chatID, nodeID, targetID string, revision int, remoteURL string) error,
) {
	client := func(ctx context.Context, chatID string) *recordstore.Client {
		if artifacts == nil {
			return nil
		}
		c := recordstore.New(artifacts, artifactref.AppName, userFor(ctx, chatID), chatID)
		if ledgerStore != nil {
			c = c.WithLedger(ledgerStore)
		}
		return c
	}
	checker = func(ctx context.Context, chatID, targetID string, revision int) (bool, error) {
		return DeliveryRecorded(ctx, client(ctx, chatID), targetID, revision)
	}
	recorder = func(ctx context.Context, chatID, nodeID, targetID string, revision int, remoteURL string) error {
		// GatePassed left false: recovery never observed the original gate
		// verdict (delivery.intent's payload doesn't carry it either) and
		// must not assert an outcome it didn't see.
		return SaveDeliveryRecord(ctx, client(ctx, chatID), nodeID, DeliveryRecord{
			TargetID: targetID, DeliveredRevision: revision, RemoteURL: remoteURL, At: time.Now().UTC(),
		})
	}
	return checker, recorder
}

// listDeliveryRecords returns every delivered revision of targetID's subject,
// as a synthetic ArtifactSummary per revision (#1093 case 8 just needs
// "count grows by one per delivery").
func listDeliveryRecords(ctx context.Context, cfg Config, targetID string) []recordstore.ArtifactSummary {
	c := recordClient(cfg)
	if c == nil {
		return nil
	}
	id := deliveryRecordID(targetID)
	versions, err := c.Versions(ctx, id)
	if err != nil {
		return nil
	}
	out := make([]recordstore.ArtifactSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, recordstore.ArtifactSummary{ID: id, Kind: kindDeliveryRecord, Revision: v})
	}
	return out
}
