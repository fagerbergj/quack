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
	"reflect"
	"sort"
	"text/tabwriter"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
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

// LedgerRebuildReport is `quack ledger rebuild`'s result: what actually
// DIFFERED from the fold (or, under --dry-run, what WOULD differ) - #1121:
// this is a diff against the current rows, never a raw candidate count, so
// it reports 0 on a chat that hasn't drifted.
type LedgerRebuildReport struct {
	ChatID                   string   `json:"chat_id"`
	DryRun                   bool     `json:"dry_run"`
	Force                    bool     `json:"force,omitempty"`
	ArtifactRevisionsChanged int      `json:"artifact_revisions_changed"`
	ArtifactUpdateErrors     []string `json:"artifact_update_errors,omitempty"`
	// SSERowsInserted: node-lifecycle rows genuinely missing from the table,
	// inserted without touching any other row. Zero in --force mode (see
	// SSERowsReplaced instead).
	SSERowsInserted int `json:"sse_rows_inserted"`
	// SSERowsReplaced: --force mode ONLY - the whole table was wiped and
	// replaced with this many synthesized rows, losing every observational
	// event (agent_token, agent_thinking, tool calls, dag_plan, ...).
	SSERowsReplaced int `json:"sse_rows_replaced,omitempty"`
}

// RunLedgerRebuild reconciles chatID's artifact store rows and SSE table
// against the ledger fold. Default (force=false, the safe path, #1121):
//   - artifact metadata: a revision's kind/class/lineage is updated ONLY if
//     it actually differs from the fold - compared via LoadWithMeta, not
//     assumed. Bytes and revision numbers are never touched, they aren't in
//     the fold.
//   - SSE table: ONLY node-lifecycle rows (node_start/node_done/node_failed)
//     that are COMPLETELY MISSING are inserted, keyed by (node id, event
//     name) - never by seq (the ledger's seq space and the table's per-run
//     seq space are different counters, see runlog.LoadEvents's doc; reusing
//     a ledger seq as a table row's Seq risks colliding with and silently
//     overwriting an unrelated real row). Every existing row - lifecycle or
//     observational - is left untouched, since the fold can't reconstruct
//     ANY row's full real content (tokens/output/model/exact timestamps) and
//     overwriting on that basis would replace real data with a placeholder.
//
// force=true is the OLD, destructive "replace the whole table" mode: it
// deletes every row (including all observational history) and rewrites the
// table from the fold alone. Only for a chat the operator has already
// decided to treat as unrecoverable any other way.
//
// dryRun computes the report without writing anything.
func RunLedgerRebuild(ctx context.Context, ls ledger.LedgerStore, st *store.Store, artifacts *store.TurnAwareService, chatID string, dryRun, force bool) (*LedgerRebuildReport, error) {
	res, err := fold.Fold(ctx, ls, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("ledger rebuild: fold chat %q: %w", chatID, err)
	}
	report := &LedgerRebuildReport{ChatID: chatID, DryRun: dryRun, Force: force}
	userID := st.SessionUserForChat(ctx, chatID)

	ids := make([]string, 0, len(res.Artifacts))
	for id := range res.Artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic report order
	for _, id := range ids {
		for _, rev := range res.Artifacts[id].Revisions {
			drifted, cerr := artifactMetaDrifted(ctx, artifacts, artifactref.AppName, userID, chatID, id, rev)
			if cerr != nil {
				report.ArtifactUpdateErrors = append(report.ArtifactUpdateErrors, fmt.Sprintf("%s@%d: %v", id, rev.Revision, cerr))
				continue
			}
			if !drifted {
				continue
			}
			report.ArtifactRevisionsChanged++
			if dryRun {
				continue
			}
			if err := artifacts.UpdateArtifactMeta(ctx, artifactref.AppName, userID, chatID, id, int64(rev.Revision), rev.Kind, rev.Class, rev.Lineage); err != nil {
				report.ArtifactUpdateErrors = append(report.ArtifactUpdateErrors, fmt.Sprintf("%s@%d: %v", id, rev.Revision, err))
			}
		}
	}

	if force {
		events := runlog.SynthesizeChatEvents(chatID, res)
		report.SSERowsReplaced = len(events)
		if !dryRun {
			if err := st.DeleteChatEvents(ctx, chatID); err != nil {
				return report, fmt.Errorf("ledger rebuild: clear SSE table for chat %q: %w", chatID, err)
			}
			for _, ev := range events {
				if err := st.InsertChatEvent(ctx, ev); err != nil {
					return report, fmt.Errorf("ledger rebuild: insert SSE row for chat %q: %w", chatID, err)
				}
			}
		}
		return report, nil
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
	missing := runlog.MissingLifecycleEvents(chatID, res, func(nodeID, name string) bool {
		return have[nodeID+"\x00"+name]
	})
	report.SSERowsInserted = len(missing)
	if !dryRun {
		now := time.Now().UTC()
		for _, ce := range missing {
			maxSeq++
			ce.Seq, ce.CreatedAt = maxSeq, now
			if err := st.InsertChatEvent(ctx, ce); err != nil {
				return report, fmt.Errorf("ledger rebuild: insert SSE row for chat %q: %w", chatID, err)
			}
		}
	}
	return report, nil
}

// artifactMetaDrifted reports whether id@revision's STORED kind/class/
// lineage differs from what the fold says it should be - the diff #1121
// requires before counting or writing anything.
func artifactMetaDrifted(ctx context.Context, artifacts *store.TurnAwareService, appName, userID, chatID, id string, rev fold.ArtifactRevision) (bool, error) {
	_, kind, class, lineageJSON, err := artifacts.LoadWithMeta(ctx, &artifact.LoadRequest{
		AppName: appName, UserID: userID, SessionID: chatID, FileName: id, Version: int64(rev.Revision),
	})
	if err != nil {
		return false, err
	}
	if kind != rev.Kind || class != rev.Class {
		return true, nil
	}
	equal, err := jsonEqual(lineageJSON, rev.Lineage)
	if err != nil {
		return true, nil // stored lineage doesn't even parse - treat as drifted
	}
	return !equal, nil
}

// jsonEqual compares two JSON documents structurally (map/slice/scalar),
// immune to key-order or whitespace differences between two independent
// marshals of the same value.
func jsonEqual(a, b json.RawMessage) (bool, error) {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false, err
	}
	return reflect.DeepEqual(va, vb), nil
}

// FormatLedgerRebuildReport renders report as the human-readable summary `rebuild` prints.
func FormatLedgerRebuildReport(r *LedgerRebuildReport) string {
	verb := "rebuilt"
	if r.DryRun {
		verb = "would rebuild"
	}
	var s string
	if r.Force {
		s = fmt.Sprintf("%s chat %s (--force): %d artifact revision(s) %s, chat_events REPLACED with %d synthesized row(s) - observational history lost\n",
			verb, r.ChatID, r.ArtifactRevisionsChanged, verbPast(r.DryRun), r.SSERowsReplaced)
	} else {
		s = fmt.Sprintf("%s chat %s: %d artifact revision(s) %s, %d SSE row(s) %s (inserted only - no row was touched or deleted)\n",
			verb, r.ChatID, r.ArtifactRevisionsChanged, verbPast(r.DryRun), r.SSERowsInserted, verbPast(r.DryRun))
	}
	for _, e := range r.ArtifactUpdateErrors {
		s += "  error: " + e + "\n"
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

// OrphanedDelivery is one delivery.intent with no matching delivery.done.
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

// findOrphanedDeliveryIntents scans chatID's ledger for delivery.intent
// entries with no later delivery.done sharing the same Key - the crash
// window #1093 case 13 covers (died between intent and done).
func findOrphanedDeliveryIntents(ctx context.Context, ls ledger.LedgerStore, chatID string) ([]OrphanedDelivery, error) {
	entries, err := ls.ReadEntries(ctx, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("ledger recover: read chat %q: %w", chatID, err)
	}
	done := map[string]bool{}
	var intents []OrphanedDelivery
	for _, e := range entries {
		switch e.Kind {
		case ledger.KindDeliveryDone:
			done[e.Key] = true
		case ledger.KindDeliveryIntent:
			var p deliveryIntentPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			intents = append(intents, OrphanedDelivery{Key: e.Key, TargetID: p.TargetID, Revision: p.Revision, NodeID: e.NodeID, Seq: e.Seq,
				CloneURL: p.CloneURL, IssueNumber: p.IssueNumber})
		}
	}
	out := intents[:0]
	for _, in := range intents {
		if !done[in.Key] {
			out = append(out, in)
		}
	}
	return out, nil
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
	Confirmed  []OrphanedDelivery `json:"confirmed"`            // delivery.done appended; extension already had it
	Redone     []OrphanedDelivery `json:"redone"`               // Redo called; nothing was there
	Unresolved []OrphanedDelivery `json:"unresolved,omitempty"` // no recoverer/Redo available to check, or dry-run
	// Aborted: artifact.revision intents with no row, now (or under
	// --dry-run, would be) marked artifact.revision.aborted so the next save
	// builds on the real parent revision.
	Aborted []OrphanedRevision `json:"aborted,omitempty"`
	Errors  []string           `json:"errors,omitempty"`
}

// unresolved counts the intents this pass could not settle - the
// quack_ledger_unresolved_intents gauge's per-chat contribution.
func (r *LedgerRecoverReport) unresolved() int {
	n := len(r.Unresolved) + len(r.Errors)
	if r.DryRun {
		n += len(r.Aborted)
	}
	return n
}

// RunLedgerRecover reconciles one chat's intents whose projection write is
// missing. Delivery (#1093 case 13): for each delivery.intent with no
// delivery.done, ask p.Delivery whether the target already saw the key; if
// so append delivery.done, else run p.Redo. Artifacts: each live
// artifact.revision with no store row gets an artifact.revision.aborted
// marker. Idempotent: a settled intent no longer shows up as an orphan.
// dryRun reports without calling anything or writing.
func RunLedgerRecover(ctx context.Context, ls ledger.LedgerStore, chatID string, p Projections, dryRun bool) (*LedgerRecoverReport, error) {
	orphans, err := findOrphanedDeliveryIntents(ctx, ls, chatID)
	if err != nil {
		return nil, err
	}
	report := &LedgerRecoverReport{ChatID: chatID, DryRun: dryRun}
	for _, o := range orphans {
		if !dryRun && p.Delivery != nil {
			dc := DeliveryContext{CloneURL: o.CloneURL, IssueNumber: o.IssueNumber}
			found, outcome, rerr := p.Delivery.RecoverDelivery(ctx, o.Key, dc)
			if rerr != nil {
				report.Unresolved = append(report.Unresolved, o)
				continue
			}
			if found {
				payload, _ := json.Marshal(struct {
					RemoteURL string `json:"remote_url,omitempty"`
				}{RemoteURL: outcome.URL})
				if _, aerr := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, NodeID: o.NodeID, Kind: ledger.KindDeliveryDone, Key: o.Key, Payload: payload}); aerr != nil {
					return nil, fmt.Errorf("ledger recover: append delivery.done for key %q: %w", o.Key, aerr)
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
	res, err := fold.Fold(ctx, ls, chatID, 0)
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
			o := OrphanedRevision{ID: id, Revision: rev.Revision, NodeID: rev.NodeID, TurnID: rev.TurnID, Seq: rev.Seq}
			report.Aborted = append(report.Aborted, o)
			if dryRun {
				continue
			}
			payload, _ := json.Marshal(struct {
				Revision int    `json:"revision"`
				Reason   string `json:"reason"`
			}{Revision: rev.Revision, Reason: "recovery: no store row for this revision"})
			if _, aerr := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, TurnID: rev.TurnID, NodeID: rev.NodeID,
				Kind: ledger.KindArtifactRevisionAborted, Key: id, Payload: payload}); aerr != nil {
				return nil, fmt.Errorf("ledger recover: append artifact.revision.aborted for %s@%d: %w", id, rev.Revision, aerr)
			}
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
// ponytail: folds every chat from seq 0 on each boot; P3's projection
// watermarks turn this into an incremental scan.
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
		if len(report.Confirmed)+len(report.Redone)+len(report.Unresolved)+len(report.Aborted)+len(report.Errors) == 0 {
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
	verb := "aborted"
	if r.DryRun {
		verb = "would abort"
	}
	s := fmt.Sprintf("chat %s: %d confirmed already-delivered, %d redelivered, %d unresolved, %d row-less revision(s) %s\n",
		r.ChatID, len(r.Confirmed), len(r.Redone), len(r.Unresolved), len(r.Aborted), verb)
	for _, o := range r.Unresolved {
		s += fmt.Sprintf("  unresolved: key=%s target=%s rev=%d node=%s seq=%d\n", o.Key, o.TargetID, o.Revision, o.NodeID, o.Seq)
	}
	for _, o := range r.Aborted {
		s += fmt.Sprintf("  %s: %s@%d node=%s seq=%d\n", verb, o.ID, o.Revision, o.NodeID, o.Seq)
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
