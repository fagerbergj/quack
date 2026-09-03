// deliveryrecord.go: the "delivery_record" kind (#1093, P6/P10 of the
// artifact-model epic #1090) - one row per delivered revision of a subject,
// written by commitDelivery after a successful Deliver/RecoverDelivery.
package vetting

import (
	"context"
	"log/slog"
	"strconv"
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
		// Instance = hint verbatim, "<subject>:<delivered_revision>" (built by
		// deliveryRecordHint below) - one entry per delivered revision, not one
		// growing row per subject, since SaveStructured always replaces rather
		// than appends: a distinct id per revision is what makes the "history"
		// append-only (List(ctx, kindDeliveryRecord) then returns every entry).
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
}

// deliveryRecordHint composes the delivery_record instance for one delivered
// revision of a subject.
func deliveryRecordHint(targetID string, revision int) string {
	return targetID + "@" + strconv.Itoa(revision)
}

// saveDeliveryRecord writes one delivery_record entry (#1093). Fail-open:
// a save error is Warned, matching every other episodic write in this
// package - the delivery itself already happened by the time this is called.
func saveDeliveryRecord(ctx context.Context, cfg Config, nodeID string, rec DeliveryRecord) {
	c := recordClient(cfg)
	if c == nil {
		return
	}
	hint := deliveryRecordHint(rec.TargetID, rec.DeliveredRevision)
	lineage := recordstore.Lineage{NodeID: nodeID, SavedAt: rec.At}
	if _, _, err := c.SaveStructured(ctx, kindDeliveryRecord, rec, hint, lineage); err != nil {
		slog.Warn("delivery_record save failed", "component", "vetting", "node", nodeID, "target", rec.TargetID, "err", err)
	}
}

// listDeliveryRecords returns every delivery_record entry for one subject,
// oldest first - #1093 case 8 just needs "count grows by one per delivery",
// so this scans List(ctx, kindDeliveryRecord) rather than building a real
// query API nobody asked for.
func listDeliveryRecords(ctx context.Context, cfg Config, targetID string) []recordstore.ArtifactSummary {
	c := recordClient(cfg)
	if c == nil {
		return nil
	}
	all, err := c.List(ctx, kindDeliveryRecord)
	if err != nil {
		return nil
	}
	prefix := kindDeliveryRecord + ":" + targetID + "@"
	out := make([]recordstore.ArtifactSummary, 0, len(all))
	for _, a := range all {
		if len(a.ID) >= len(prefix) && a.ID[:len(prefix)] == prefix {
			out = append(out, a)
		}
	}
	return out
}
