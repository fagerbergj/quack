// TUI-free logic behind `quack ledger show`/`rebuild` (V4 §4.9/#1101). There
// is no REST surface for raw ledger entries or a rebuild trigger (that's a
// later API step, and internal/server/rest's artifact endpoints are another
// change in flight) - both commands run server-side, against the SAME
// stores a local `quack.yaml` would boot `quack serve` against (see
// cmd/quack/ledger.go, mirroring how `quack replay` builds an in-process
// server from local config instead of talking to a running one).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
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

// LedgerRebuildReport is `quack ledger rebuild`'s result: what changed (or,
// under --dry-run, what WOULD change).
type LedgerRebuildReport struct {
	ChatID                   string   `json:"chat_id"`
	DryRun                   bool     `json:"dry_run"`
	ArtifactRevisionsUpdated int      `json:"artifact_revisions_updated"`
	ArtifactUpdateErrors     []string `json:"artifact_update_errors,omitempty"`
	SSEEventsWritten         int      `json:"sse_events_written"`
}

// RunLedgerRebuild regenerates chatID's artifact store rows (kind/class/
// lineage only - bytes and revision numbers are never touched, they aren't
// in the fold) and its SSE table (node_start/node_done/node_failed,
// reconstructed from node.* entries - see runlog.SynthesizeChatEvents's doc
// for what that reconstruction loses) from the ledger fold. dryRun computes
// the report without writing anything.
func RunLedgerRebuild(ctx context.Context, ls ledger.LedgerStore, st *store.Store, artifacts *store.TurnAwareService, chatID string, dryRun bool) (*LedgerRebuildReport, error) {
	res, err := fold.Fold(ctx, ls, chatID, 0)
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
			report.ArtifactRevisionsUpdated++
			if dryRun {
				continue
			}
			if err := artifacts.UpdateArtifactMeta(ctx, artifactref.AppName, userID, chatID, id, int64(rev.Revision), rev.Kind, rev.Class, rev.Lineage); err != nil {
				report.ArtifactUpdateErrors = append(report.ArtifactUpdateErrors, fmt.Sprintf("%s@%d: %v", id, rev.Revision, err))
			}
		}
	}

	events := runlog.SynthesizeChatEvents(chatID, res)
	report.SSEEventsWritten = len(events)
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

// FormatLedgerRebuildReport renders report as the human-readable summary `rebuild` prints.
func FormatLedgerRebuildReport(r *LedgerRebuildReport) string {
	verb := "rebuilt"
	if r.DryRun {
		verb = "would rebuild"
	}
	s := fmt.Sprintf("%s chat %s: %d artifact revision(s) %s, %d SSE event(s) %s\n",
		verb, r.ChatID, r.ArtifactRevisionsUpdated, verbPast(r.DryRun), r.SSEEventsWritten, verbPast(r.DryRun))
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
// reports whether it was already posted. This is a LOCAL copy of the
// interface an extension implements (github's App.RecoverDelivery, in
// quack-extensions) - not an import, since core is pinned to sdk v0.8.0 on
// origin/main, which predates this capability. Structurally identical to
// sdk.DeliveryRecoverer's 3-arg/DeliveryItemOutcome signature so the concrete
// extension value satisfies it by method-set shape alone with no go.mod
// bump. Delete this and import sdk.DeliveryRecoverer directly once quack's
// go.mod bumps to the sdk/github v0.9.0 release that adds it (#1093
// deploy-order note; see PR body for the exact pin steps).
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

// LedgerRecoverReport is `quack ledger recover`'s result.
type LedgerRecoverReport struct {
	ChatID     string             `json:"chat_id"`
	Confirmed  []OrphanedDelivery `json:"confirmed"`            // delivery.done appended; extension already had it
	Redone     []OrphanedDelivery `json:"redone"`               // redoFunc called; nothing was there
	Unresolved []OrphanedDelivery `json:"unresolved,omitempty"` // no recoverer/redoFunc available to check
}

// RunLedgerRecover reconciles chatID's orphaned delivery.intent entries
// (#1093 case 13): for each, ask recoverer whether the target already saw
// this key posted (a crash after Deliver but before delivery.done landed).
// If found, delivery.done is appended and redoFunc is never called - the
// extension is never asked to post twice. If not found (a crash BEFORE
// Deliver reached the extension), redoFunc runs the same delivery path
// again. recoverer/redoFunc nil means that check/action isn't wired for this
// caller (e.g. no extension implements it yet) - such intents are reported
// Unresolved rather than guessed at.
func RunLedgerRecover(ctx context.Context, ls ledger.LedgerStore, chatID string, recoverer DeliveryRecoverer, redoFunc func(ctx context.Context, o OrphanedDelivery) error) (*LedgerRecoverReport, error) {
	orphans, err := findOrphanedDeliveryIntents(ctx, ls, chatID)
	if err != nil {
		return nil, err
	}
	report := &LedgerRecoverReport{ChatID: chatID}
	for _, o := range orphans {
		if recoverer != nil {
			dc := DeliveryContext{CloneURL: o.CloneURL, IssueNumber: o.IssueNumber}
			found, outcome, rerr := recoverer.RecoverDelivery(ctx, o.Key, dc)
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
		if redoFunc != nil {
			if rerr := redoFunc(ctx, o); rerr != nil {
				report.Unresolved = append(report.Unresolved, o)
				continue
			}
			report.Redone = append(report.Redone, o)
			continue
		}
		report.Unresolved = append(report.Unresolved, o)
	}
	return report, nil
}

// FormatLedgerRecoverReport renders report as the human-readable summary `recover` prints.
func FormatLedgerRecoverReport(r *LedgerRecoverReport) string {
	s := fmt.Sprintf("chat %s: %d confirmed already-delivered, %d redelivered, %d unresolved\n",
		r.ChatID, len(r.Confirmed), len(r.Redone), len(r.Unresolved))
	for _, o := range r.Unresolved {
		s += fmt.Sprintf("  unresolved: key=%s target=%s rev=%d node=%s seq=%d\n", o.Key, o.TargetID, o.Revision, o.NodeID, o.Seq)
	}
	return s
}
