// deliveryrecord.go: the "delivery_record" kind (#1093, P6/P10 of the
// artifact-model epic #1090) - one id per subject (delivery_record:<subject>),
// one revision per delivery, appended via the normal SaveStructured
// revision-append mechanism (recordstore's save() always appends the next
// revision under a lock - the same one code_review uses).
package vetting

import (
	"context"
	"log/slog"
	"strings"
	"time"

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
	c := recordClient(cfg)
	if c == nil {
		return
	}
	lineage := recordstore.Lineage{NodeID: nodeID, SavedAt: rec.At}
	if _, _, err := c.SaveStructured(ctx, kindDeliveryRecord, rec, deliverySubject(rec.TargetID), lineage); err != nil {
		slog.Warn("delivery_record save failed", "component", "vetting", "node", nodeID, "target", rec.TargetID, "err", err)
	}
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
