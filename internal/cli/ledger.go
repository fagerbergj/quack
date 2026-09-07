// TUI-free logic behind `quack ledger`. show/rebuild/recover run against the
// SAME stores a local `quack.yaml` would boot `quack serve` against (see
// cmd/quack/ledger.go); list/export talk to a running server's REST API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"gorm.io/gorm"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// RunLedgerShow prints chatID's raw ledger entries (seq >= fromSeq) to out,
// one JSON object per line - pipeable per the quack-cli skill's "content to
// stdout" rule.
func RunLedgerShow(ctx context.Context, out io.Writer, ls ledger.LedgerStore, chatID string, fromSeq int64) error {
	entries, err := ls.ReadEntries(ctx, chatID, fromSeq)
	if err != nil {
		return fmt.Errorf("ledger show: %w", err)
	}
	enc := json.NewEncoder(out)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("ledger show: encode entry seq %d: %w", e.Seq, err)
		}
	}
	return nil
}

// LedgerRebuildReport is `quack ledger rebuild`'s result (#1144 P3: rebuild
// is now "reset the watermark to 0 and fold" - no more diff heuristics, the
// watermark itself says how much was already reconciled).
type LedgerRebuildReport struct {
	ChatID                   string   `json:"chat_id"`
	DryRun                   bool     `json:"dry_run"`
	ArtifactRevisionsChanged int      `json:"artifact_revisions_changed"`
	ArtifactUpdateErrors     []string `json:"artifact_update_errors,omitempty"`
	SSERowsInserted          int      `json:"sse_rows_inserted"`
	NodeStatesChanged        int      `json:"node_states_changed"`
	// NodeStateSkippedMultiPlan is true when node_state was left untouched
	// because chatID has more than one plan: res.Nodes folds the whole chat
	// lifetime by bare node ID, and a node ID legitimately recurs across
	// plans/turns (e.g. the auto-appended "synthesize" node) with no
	// persisted plan<->invocation mapping to attribute a terminal event back
	// to the plan it belongs to - attributing it to "the latest plan" would
	// silently fabricate or stomp state for a plan that never ran that node.
	NodeStateSkippedMultiPlan bool `json:"node_state_skipped_multi_plan,omitempty"`
}

// RunLedgerRebuild resets chatID's watermarks to 0 and folds: every artifact
// revision's kind/class/lineage is rewritten from the fold (unconditionally
// - no drift diff, the watermark reset already says "start over"), and the
// SSE table is repopulated the same way LoadEvents' resume path would
// (runlog.foldSSEFromWatermark), inserting only what the fold has that the
// table doesn't yet. dryRun computes the report without writing or
// resetting anything.
func RunLedgerRebuild(ctx context.Context, ls ledger.LedgerStore, st *store.Store, artifacts *store.TurnAwareService, chatID string, dryRun bool) (*LedgerRebuildReport, error) {
	if !dryRun {
		for _, projection := range []string{"artifact", "sse", "node_state"} {
			if err := st.ResetProjectionWatermark(ctx, chatID, projection); err != nil {
				return nil, fmt.Errorf("ledger rebuild: reset %s watermark for chat %q: %w", projection, chatID, err)
			}
		}
	}
	res, err := fold.Apply(ctx, ls, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("ledger rebuild: fold chat %q: %w", chatID, err)
	}
	report := &LedgerRebuildReport{ChatID: chatID, DryRun: dryRun}
	userID := st.SessionUserForChat(ctx, chatID)

	ids := make([]string, 0, len(res.Artifacts))
	for id := range res.Artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic report order
	for _, id := range ids {
		for _, rev := range res.Artifacts[id].Revisions {
			report.ArtifactRevisionsChanged++
			if dryRun {
				continue
			}
			if err := artifacts.UpdateArtifactMeta(ctx, artifactref.AppName, userID, chatID, id, int64(rev.Revision), rev.Kind, rev.Class, rev.Lineage); err != nil {
				report.ArtifactUpdateErrors = append(report.ArtifactUpdateErrors, fmt.Sprintf("%s@%d: %v", id, rev.Revision, err))
			}
		}
	}
	if !dryRun {
		// Artifact rows live behind the ADK artifact.Service, which may be a
		// wholly separate Postgres connection from this Store's own db (a
		// dedicated NewArtifactService URL) - unlike SSE/node_state below,
		// there is no single transaction that can span both, so the
		// watermark advance is sequential-after, not atomic-with, the row
		// writes. A crash in the gap just means the next rebuild reprocesses
		// revisions it already wrote - UpdateArtifactMeta is an
		// unconditional overwrite, so that's a harmless no-op re-write, not
		// a correctness bug.
		if err := st.SetProjectionWatermark(ctx, chatID, "artifact", res.LastSeq); err != nil {
			return report, fmt.Errorf("ledger rebuild: advance artifact watermark for chat %q: %w", chatID, err)
		}
	}

	planCount, cerr := st.CountDagPlans(ctx, chatID)
	if cerr != nil {
		return report, fmt.Errorf("ledger rebuild: count plans for chat %q: %w", chatID, cerr)
	}
	if planCount > 1 {
		// See NodeStateSkippedMultiPlan's doc: with >1 plan there is no way
		// to tell which plan a folded node.* entry belongs to, so node_state
		// is left as-is rather than guessing.
		report.NodeStateSkippedMultiPlan = true
	}
	planID, perr := st.GetLatestDagPlan(ctx, chatID)
	if perr != nil {
		return report, fmt.Errorf("ledger rebuild: latest plan for chat %q: %w", chatID, perr)
	}
	if planID != nil && planCount <= 1 {
		// Only write ids the plan actually declares (kills the phantom
		// case), same check loadPlanNode uses to 404 an unknown node.
		var planData stream.DagPlanData
		if uerr := json.Unmarshal([]byte(planID.PlanJSON), &planData); uerr != nil {
			return report, fmt.Errorf("ledger rebuild: parse latest plan JSON for chat %q: %w", chatID, uerr)
		}
		declared := make(map[string]bool, len(planData.Nodes))
		for _, n := range planData.Nodes {
			declared[n.ID] = true
		}
		nodeIDs := make([]string, 0, len(res.Nodes))
		for id := range res.Nodes {
			if declared[id] && res.Nodes[id].TerminalStatus != "" {
				nodeIDs = append(nodeIDs, id)
			}
		}
		sort.Strings(nodeIDs) // deterministic report order
		report.NodeStatesChanged = len(nodeIDs)
		if !dryRun {
			err = st.InTx(ctx, func(tx *gorm.DB) error {
				for _, id := range nodeIDs {
					n := res.Nodes[id]
					if werr := store.UpsertNodeTerminalStatusTx(tx, planID.ID, n.NodeID, n.TerminalStatus); werr != nil {
						return fmt.Errorf("node %s: %w", n.NodeID, werr)
					}
				}
				return store.SetProjectionWatermarkTx(tx, chatID, "node_state", res.LastSeq)
			})
			if err != nil {
				return report, fmt.Errorf("ledger rebuild: write node_state for chat %q: %w", chatID, err)
			}
		}
	}

	existing, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		return report, fmt.Errorf("ledger rebuild: load chat_events for chat %q: %w", chatID, err)
	}
	have := map[string]bool{}
	var maxSeq int64
	for _, row := range existing {
		if row.Seq > maxSeq {
			maxSeq = row.Seq
		}
		ev, uerr := runlog.UnmarshalEvent(row.Event)
		if uerr != nil {
			continue
		}
		if nodeID, ok := runlog.EventNodeID(ev); ok {
			have[nodeID+"\x00"+ev.Name] = true
		}
	}
	var missing []store.ChatEvent
	for _, ce := range runlog.SynthesizeChatEvents(chatID, res) {
		ev, uerr := runlog.UnmarshalEvent(ce.Event)
		if uerr != nil {
			continue
		}
		nodeID, ok := runlog.EventNodeID(ev)
		if !ok || have[nodeID+"\x00"+ev.Name] {
			continue
		}
		missing = append(missing, ce)
	}
	report.SSERowsInserted = len(missing)
	if !dryRun {
		now := time.Now().UTC()
		err = st.InTx(ctx, func(tx *gorm.DB) error {
			for _, ce := range missing {
				maxSeq++
				ce.Seq, ce.CreatedAt = maxSeq, now
				if werr := store.InsertChatEventTx(tx, ce); werr != nil {
					return werr
				}
			}
			return store.SetProjectionWatermarkTx(tx, chatID, "sse", res.LastSeq)
		})
		if err != nil {
			return report, fmt.Errorf("ledger rebuild: write sse rows for chat %q: %w", chatID, err)
		}
	}
	return report, nil
}

// FormatLedgerRebuildReport renders report as the human-readable summary `rebuild` prints.
func FormatLedgerRebuildReport(r *LedgerRebuildReport) string {
	verb := "rebuilt"
	if r.DryRun {
		verb = "would rebuild"
	}
	s := fmt.Sprintf("%s chat %s: %d artifact revision(s) %s, %d SSE row(s) %s (inserted only - no row was touched or deleted), %d node state(s) %s\n",
		verb, r.ChatID, r.ArtifactRevisionsChanged, verbPast(r.DryRun), r.SSERowsInserted, verbPast(r.DryRun), r.NodeStatesChanged, verbPast(r.DryRun))
	for _, e := range r.ArtifactUpdateErrors {
		s += "  error: " + e + "\n"
	}
	if r.NodeStateSkippedMultiPlan {
		s += "  node_state skipped: chat has more than one plan, and a node id can't be attributed to one\n"
	}
	return s
}

func verbPast(dryRun bool) string {
	if dryRun {
		return "pending"
	}
	return "written"
}

// DeliveryItemOutcome is a LOCAL copy of sdk.DeliveryItemOutcome's shape -
// see DeliveryRecoverer below for why this stays a shim, not an import.
type DeliveryItemOutcome struct {
	Kind  string
	URL   string
	Error string
}

// DeliveryContext is a LOCAL copy of the sdk.DeliveryContext fields a
// recoverer needs to look an idempotency key up (clone/PR coordinates) -
// rebuilt by RunLedgerRecover from the delivery.intent payload, since
// offline recovery has no live worker activity to derive them from.
type DeliveryContext struct {
	CloneURL    string
	IssueNumber int
}

// DeliveryRecoverer looks an idempotency key up at the delivery target (a
// hidden marker in a GitHub review body, a reMarkable document id) and
// reports whether it was already posted. This is a LOCAL copy of
// sdk.DeliveryRecoverer's shape - cli doesn't import quack-extensions/sdk
// directly (that dependency stays in internal/serve, which already adapts
// the SDK boundary elsewhere); internal/serve.sdkRecoverAdapter bridges the
// real extension's sdk.DeliveryRecoverer to this interface for
// `quack ledger recover`.
type DeliveryRecoverer interface {
	RecoverDelivery(ctx context.Context, key string, dc DeliveryContext) (found bool, outcome DeliveryItemOutcome, err error)
}

// DeliveryRecordChecker reports whether targetID's delivery_record already
// carries a successful revision for revision - the single "is this delivery
// done" read (#1144 P2), shared by boot recovery and `quack ledger recover`.
type DeliveryRecordChecker func(ctx context.Context, chatID, targetID string, revision int) (bool, error)

// DeliveryRecorder persists the delivery_record revision that completes a
// delivery.intent when the extension confirms it already landed but the
// record write itself was lost (crash between Deliver and saveDeliveryRecord).
type DeliveryRecorder func(ctx context.Context, chatID, nodeID, targetID string, revision int, remoteURL string) error

// OrphanedDelivery is one delivery.intent with no matching delivery_record.
type OrphanedDelivery struct {
	Key      string `json:"key"`
	TargetID string `json:"target_id"`
	Revision int    `json:"revision"`
	NodeID   string `json:"node_id"`
	Seq      int64  `json:"seq"`
	// CloneURL/IssueNumber: minimal DeliveryContext fields persisted in the
	// delivery.intent payload (#1093 finding 4) - enough to rebuild a
	// DeliveryContext for a recoverer offline, without live worker activity.
	CloneURL    string `json:"clone_url,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
}

type deliveryIntentPayload struct {
	TargetID    string `json:"target_id"`
	Revision    int    `json:"revision"`
	Key         string `json:"idempotency_key"`
	CloneURL    string `json:"clone_url,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
}

// findDeliveryIntents scans chatID's ledger for every delivery.intent entry.
// Whether one is already settled is a DeliveryRecordChecker read against the
// delivery_record artifact (#1144 P2), not a second ledger entry kind.
func findDeliveryIntents(ctx context.Context, ls ledger.LedgerStore, chatID string) ([]OrphanedDelivery, error) {
	entries, err := ls.ReadEntries(ctx, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("ledger recover: read chat %q: %w", chatID, err)
	}
	var intents []OrphanedDelivery
	for _, e := range entries {
		if e.Kind != ledger.KindDeliveryIntent {
			continue
		}
		var p deliveryIntentPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		intents = append(intents, OrphanedDelivery{Key: e.Key, TargetID: p.TargetID, Revision: p.Revision, NodeID: e.NodeID, Seq: e.Seq,
			CloneURL: p.CloneURL, IssueNumber: p.IssueNumber})
	}
	return intents, nil
}

// OrphanedRevision is one artifact.revision intent whose store row never
// materialized (a crash between the WAL append and the row write).
type OrphanedRevision struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	NodeID   string `json:"node_id"`
	TurnID   string `json:"turn_id"`
	Seq      int64  `json:"seq"`
}

// Projections is what Recover checks each intent against. A nil member
// skips that intent family (its orphans are reported, never touched).
type Projections struct {
	// ArtifactRowExists reports whether id@revision has a store row.
	ArtifactRowExists func(ctx context.Context, chatID, id string, revision int) (bool, error)
	Delivery          DeliveryRecoverer
	DeliveryRecorded  DeliveryRecordChecker
	RecordDelivery    DeliveryRecorder
	Redo              func(ctx context.Context, o OrphanedDelivery) error
}

// ArtifactRowChecker adapts the artifact store to Projections.ArtifactRowExists.
func ArtifactRowChecker(st *store.Store, artifacts *store.TurnAwareService) func(context.Context, string, string, int) (bool, error) {
	return func(ctx context.Context, chatID, id string, revision int) (bool, error) {
		return artifacts.RevisionExists(ctx, artifactref.AppName, st.SessionUserForChat(ctx, chatID), chatID, id, int64(revision))
	}
}

// LedgerRecoverReport is one chat's recovery result.
type LedgerRecoverReport struct {
	ChatID     string             `json:"chat_id"`
	DryRun     bool               `json:"dry_run,omitempty"`
	Confirmed  []OrphanedDelivery `json:"confirmed"`            // delivery_record recorded; extension already had it
	Redone     []OrphanedDelivery `json:"redone"`               // Redo called; nothing was there
	Unresolved []OrphanedDelivery `json:"unresolved,omitempty"` // no recoverer/Redo available to check, or dry-run
	// OrphanedRevisions: artifact.revision intents with no store row. #1144
	// P4 deleted the artifact.revision.aborted compensating marker - a
	// PLAIN SAVE on the same id now self-heals this by adopting the orphan
	// (recordstore.saveAtOrAdopt), so recovery only reports it (unresolved
	// count), it never writes.
	OrphanedRevisions []OrphanedRevision `json:"orphaned_revisions,omitempty"`
	Errors            []string           `json:"errors,omitempty"`
}

// unresolved counts the intents this pass could not settle - the
// quack_ledger_unresolved_intents gauge's per-chat contribution.
func (r *LedgerRecoverReport) unresolved() int {
	return len(r.Unresolved) + len(r.Errors) + len(r.OrphanedRevisions)
}

// RunLedgerRecover reconciles one chat's intents whose projection write is
// missing. Delivery (#1093 case 13, #1144 P2): a delivery.intent is settled
// once its delivery_record artifact revision exists (p.DeliveryRecorded) -
// no separate ledger entry to check. For an unsettled one, ask p.Delivery
// whether the target already saw the key; if so, p.RecordDelivery writes the
// completion, else run p.Redo. Artifacts: each live artifact.revision with no
// store row is reported (OrphanedRevisions) - recordstore's own retry path
// self-heals it on the next save to that id (see saveAtOrAdopt), so recovery
// only surfaces it, never writes. Idempotent: a settled intent no longer
// shows up as an orphan. dryRun changes only the delivery half.
func RunLedgerRecover(ctx context.Context, ls ledger.LedgerStore, chatID string, p Projections, dryRun bool) (*LedgerRecoverReport, error) {
	intents, err := findDeliveryIntents(ctx, ls, chatID)
	if err != nil {
		return nil, err
	}
	report := &LedgerRecoverReport{ChatID: chatID, DryRun: dryRun}
	for _, o := range intents {
		if p.DeliveryRecorded != nil {
			done, derr := p.DeliveryRecorded(ctx, chatID, o.TargetID, o.Revision)
			if derr == nil && done {
				continue // settled - not orphaned
			}
		}
		if !dryRun && p.Delivery != nil {
			dc := DeliveryContext{CloneURL: o.CloneURL, IssueNumber: o.IssueNumber}
			found, outcome, rerr := p.Delivery.RecoverDelivery(ctx, o.Key, dc)
			if rerr != nil {
				report.Unresolved = append(report.Unresolved, o)
				continue
			}
			if found {
				if p.RecordDelivery != nil {
					if aerr := p.RecordDelivery(ctx, chatID, o.NodeID, o.TargetID, o.Revision, outcome.URL); aerr != nil {
						return nil, fmt.Errorf("ledger recover: record delivery for key %q: %w", o.Key, aerr)
					}
				}
				report.Confirmed = append(report.Confirmed, o)
				continue
			}
		}
		if !dryRun && p.Redo != nil {
			if rerr := p.Redo(ctx, o); rerr != nil {
				report.Unresolved = append(report.Unresolved, o)
				continue
			}
			report.Redone = append(report.Redone, o)
			continue
		}
		report.Unresolved = append(report.Unresolved, o)
	}
	if p.ArtifactRowExists == nil {
		return report, nil
	}
	res, err := fold.Apply(ctx, ls, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("ledger recover: fold chat %q: %w", chatID, err)
	}
	ids := make([]string, 0, len(res.Artifacts))
	for id := range res.Artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, rev := range res.Artifacts[id].Revisions {
			exists, cerr := p.ArtifactRowExists(ctx, chatID, id, rev.Revision)
			if cerr != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s@%d: %v", id, rev.Revision, cerr))
				continue
			}
			if exists {
				continue
			}
			report.OrphanedRevisions = append(report.OrphanedRevisions, OrphanedRevision{
				ID: id, Revision: rev.Revision, NodeID: rev.NodeID, TurnID: rev.TurnID, Seq: rev.Seq,
			})
		}
	}
	return report, nil
}

// RecoverSummary is Recover's whole-ledger result.
type RecoverSummary struct {
	Chats      int                    `json:"chats"`
	Unresolved int                    `json:"unresolved"`
	Reports    []*LedgerRecoverReport `json:"reports"`
}

// Recover runs RunLedgerRecover over every chat in ls (or only chatIDs when
// given), publishes quack_ledger_unresolved_intents and logs a summary. It
// runs at server boot; `quack ledger recover` is the same call with dryRun.
// ponytail: folds every chat from seq 0 on each boot - P3's watermarks
// gate SSE/artifact/node_state writes, not this recover path; make it
// incremental if boot-time recover cost ever matters.
func Recover(ctx context.Context, ls ledger.LedgerStore, chatIDs []string, p Projections, dryRun bool) (*RecoverSummary, error) {
	if len(chatIDs) == 0 {
		refs, err := ls.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("ledger recover: list chats: %w", err)
		}
		for _, r := range refs {
			chatIDs = append(chatIDs, r.ID)
		}
	}
	sum := &RecoverSummary{Chats: len(chatIDs)}
	for _, id := range chatIDs {
		report, err := RunLedgerRecover(ctx, ls, id, p, dryRun)
		if err != nil {
			return nil, err
		}
		if len(report.Confirmed)+len(report.Redone)+len(report.Unresolved)+len(report.OrphanedRevisions)+len(report.Errors) == 0 {
			continue
		}
		sum.Reports = append(sum.Reports, report)
		sum.Unresolved += report.unresolved()
	}
	otelobs.SetLedgerUnresolvedIntents(int64(sum.Unresolved))
	slog.Info("ledger recovery", "component", "ledger", "dry_run", dryRun, "chats", sum.Chats, "chats_with_orphans", len(sum.Reports), "unresolved", sum.Unresolved)
	return sum, nil
}

// FormatLedgerRecoverReport renders report as the human-readable summary `recover` prints.
func FormatLedgerRecoverReport(r *LedgerRecoverReport) string {
	s := fmt.Sprintf("chat %s: %d confirmed already-delivered, %d redelivered, %d unresolved, %d row-less revision(s) (self-heals on next save)\n",
		r.ChatID, len(r.Confirmed), len(r.Redone), len(r.Unresolved), len(r.OrphanedRevisions))
	for _, o := range r.Unresolved {
		s += fmt.Sprintf("  unresolved: key=%s target=%s rev=%d node=%s seq=%d\n", o.Key, o.TargetID, o.Revision, o.NodeID, o.Seq)
	}
	for _, o := range r.OrphanedRevisions {
		s += fmt.Sprintf("  no store row: %s@%d node=%s seq=%d\n", o.ID, o.Revision, o.NodeID, o.Seq)
	}
	for _, e := range r.Errors {
		s += "  error: " + e + "\n"
	}
	return s
}

// FormatRecoverSummary renders every chat that had something to recover.
func FormatRecoverSummary(sum *RecoverSummary) string {
	s := fmt.Sprintf("%d chat(s) scanned, %d with orphaned intents, %d unresolved\n", sum.Chats, len(sum.Reports), sum.Unresolved)
	for _, r := range sum.Reports {
		s += FormatLedgerRecoverReport(r)
	}
	return s
}

// RunLedgerList is `quack ledger list`: chats the server's ledger has
// observation entries for (id, entry count, last activity), or raw JSON.
func RunLedgerList(ctx context.Context, out io.Writer, server string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	recs, err := c.ListRecordings(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("no ledger store is configured on this server")
		}
		return err
	}
	if asJSON {
		return writeJSON(out, recs)
	}
	if len(recs) == 0 {
		fmt.Fprintln(out, "No recordings yet.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHAT ID\tENTRIES\tMODIFIED")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.ChatId, r.SizeBytes, r.ModifiedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

// RunLedgerExport is `quack ledger export <chat-id> [-o file]`: downloads
// the chat's recording bundle to outFile (default "<chat-id>.zip").
func RunLedgerExport(ctx context.Context, out io.Writer, server, chatID, outFile string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if outFile == "" {
		outFile = chatID + ".zip"
	}
	body, err := c.FetchRecording(ctx, chatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("no recording for chat %s (never recorded, hard-deleted, or recording.observations off)", chatID)
		}
		return err
	}
	if err := os.WriteFile(outFile, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outFile, err)
	}
	fmt.Fprintln(out, outFile)
	return nil
}

// humanSize renders n bytes as a short human-readable size (B/KB/MB/GB).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
