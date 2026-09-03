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
