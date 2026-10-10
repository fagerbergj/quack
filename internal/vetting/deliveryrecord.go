// deliveryrecord.go: the "delivery_record" kind - one id per subject, one revision per delivery.
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
		// Gate-written only: AgentWritable false keeps a write_delivery_record tool from any worker.
		JSONSchema: `{"type":"object"}`,
		Validate:   validateJSONObject[DeliveryRecord],
		// Instance = the subject (e.g. "pr:123"): every delivery of it is a revision of one id,
		// so history is one id's revisions, not a prefix scan.
		Identity:     func(_ []byte, hint string) (string, error) { return requireHint(hint) },
		RequiresHint: true,
	})
}

// DeliveryRecord: the "delivery_record" body. RemoteURL is what every DeliveryItemOutcome
// already reports; PRNumber comes from dc.IssueNumber.
type DeliveryRecord struct {
	TargetID          string    `json:"target_id"`
	DeliveredRevision int       `json:"delivered_revision"`
	RemoteURL         string    `json:"remote_url,omitempty"`
	PRNumber          int       `json:"pr_number,omitempty"`
	At                time.Time `json:"at"`
	// GatePassed: false on a gate-fail draft delivery; DeliveredRevision is still the artifact
	// revision actually rendered and posted.
	GatePassed bool `json:"gate_passed"`
	// RenderedFromStaged: the worker's staged text was posted, not an artifact render; must never
	// be diffed as artifact-backed for carried-over/resolved findings.
	RenderedFromStaged bool `json:"rendered_from_staged,omitempty"`
	// Error: non-empty on a failed attempt, kept so a later revision isn't mistaken for the first.
	Error string `json:"error,omitempty"`
	// HeadSHA: the reviewed clone's HEAD at delivery; a re-review's "N commits since" line reads
	// the prior delivery's.
	HeadSHA string `json:"head_sha,omitempty"`
}

// deliverySubject strips the leading "<kind>:" ("code_review:pr:123" -> "pr:123"): one
// delivery_record id per subject, whatever kind triggered it.
func deliverySubject(targetID string) string {
	if _, subject, ok := strings.Cut(targetID, ":"); ok {
		return subject
	}
	return targetID
}

func deliveryRecordID(targetID string) string {
	return kindDeliveryRecord + ":" + deliverySubject(targetID)
}

// saveDeliveryRecord is fail-open (Warn only): the delivery itself already happened.
func saveDeliveryRecord(ctx context.Context, cfg Config, nodeID string, rec DeliveryRecord) {
	if err := SaveDeliveryRecord(ctx, recordClient(cfg), nodeID, rec); err != nil {
		slog.Warn("delivery_record save failed", "component", "vetting", "node", nodeID, "target", rec.TargetID, "err", err)
	}
}

// SaveDeliveryRecord is the WAL's completion for a delivery.intent, exported for boot and
// `quack ledger recover`, which have a client but no Config. nil client is a no-op.
func SaveDeliveryRecord(ctx context.Context, c *recordstore.Client, nodeID string, rec DeliveryRecord) error {
	if c == nil {
		return nil
	}
	lineage := recordstore.Lineage{NodeID: nodeID, SavedAt: rec.At}
	_, _, err := c.SaveStructured(ctx, kindDeliveryRecord, rec, deliverySubject(rec.TargetID), lineage)
	return err
}

// DeliveryRecorded: any successful revision in targetID's history records revision. Walks it all,
// since a subject delivered twice has older settled intents that are no longer Latest.
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

// DeliveryProjections: delivery_record checker/recorder for recovery without a live Config;
// userFor resolves the chat's owning user.
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
		// GatePassed left false: recovery never saw the gate verdict, so must not assert one.
		return SaveDeliveryRecord(ctx, client(ctx, chatID), nodeID, DeliveryRecord{
			TargetID: targetID, DeliveredRevision: revision, RemoteURL: remoteURL, At: time.Now().UTC(),
		})
	}
	return checker, recorder
}

// latestDeliveryRecord: the PRIOR delivery for targetID's subject, since this round's record is
// written after Deliver. false on a first-ever review.
func latestDeliveryRecord(ctx context.Context, cfg Config, targetID string) (DeliveryRecord, bool) {
	c := recordClient(cfg)
	if c == nil {
		return DeliveryRecord{}, false
	}
	raw, _, ok, err := c.Latest(ctx, deliveryRecordID(targetID))
	if err != nil || !ok {
		return DeliveryRecord{}, false
	}
	var rec DeliveryRecord
	if json.Unmarshal(raw, &rec) != nil {
		return DeliveryRecord{}, false
	}
	return rec, true
}

// listDeliveryRecords: one synthetic ArtifactSummary per delivered revision of targetID's subject.
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
