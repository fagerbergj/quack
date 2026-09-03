// deliveryartifact.go: renders a round's delivery from the durable
// code_review/finding/pr_body records instead of the worker's own staged
// text (#1093, P6/P10 of the artifact-model epic #1090). commitDelivery
// calls this on every final round, passed or failed - a code_review/document
// revision is written every round (saveEpisodicRound), so the posted content
// and the recorded delivered_revision are always the same thing, even on a
// gate-fail draft delivery (design V4 §4.5).
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// artifactRenderedDelivery replaces the "review" and "pr" staged items with
// artifact-backed renders where a record exists, leaving every other staged
// item (and either one, on any render failure) exactly as the worker staged
// it. staged is mutated in place and returned for call-site convenience;
// fromStaged is true when any item fell back to the worker's own staged text
// (finding 2: the caller must never record such a delivery as artifact-backed).
func artifactRenderedDelivery(ctx context.Context, cfg Config, nodeID string, staged map[string]StagedDelivery) (result map[string]StagedDelivery, fromStaged bool) {
	if cfg.IsReviewer {
		if item, ok := renderReviewFromArtifact(ctx, cfg, nodeID); ok {
			staged["review"] = item
		} else {
			slog.Warn("no passed code_review artifact revision; delivering staged review text", "component", "vetting", "node", nodeID)
			fromStaged = true
		}
	}
	if item, hasPR := staged["pr"]; hasPR {
		if rendered, ok := renderPRBodyFromArtifact(ctx, cfg, nodeID, item); ok {
			staged["pr"] = rendered
		} else {
			slog.Warn("no pr_body artifact; delivering staged PR text", "component", "vetting", "node", nodeID)
			fromStaged = true
		}
	}
	return staged, fromStaged
}

// renderReviewFromArtifact loads the latest code_review record (only valid
// to call once the round it belongs to has passed - see commitDelivery) and
// its findings, and renders the same StagedDelivery{Kind: "review", ...}
// shape stage_review/the answer-tail parser would have produced. false when
// no code_review record exists yet for this subject (fresh chat, or a
// non-episodic reviewer config) - the caller falls back to staged text.
func renderReviewFromArtifact(ctx context.Context, cfg Config, nodeID string) (StagedDelivery, bool) {
	c := recordClient(cfg)
	if c == nil {
		return StagedDelivery{}, false
	}
	id, err := recordstore.IdentityFor(kindCodeReview, nil, SubjectHint(cfg.ChatID))
	if err != nil {
		return StagedDelivery{}, false
	}
	raw, rev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok {
		return StagedDelivery{}, false
	}
	var rec CodeReviewRecord
	if json.Unmarshal(raw, &rec) != nil {
		return StagedDelivery{}, false
	}

	// First-ever delivered revision for this subject: nothing to carry over.
	firstDelivery := len(listDeliveryRecords(ctx, cfg, id)) == 0

	var comments []ReviewComment
	var newIDs, carriedIDs, resolvedIDs []string
	for _, fid := range rec.FindingIDs {
		fRaw, _, fok, ferr := c.Latest(ctx, fid)
		if ferr != nil || !fok {
			continue
		}
		var f FindingRecord
		if json.Unmarshal(fRaw, &f) != nil {
			continue
		}
		switch {
		case f.State == "resolved":
			resolvedIDs = append(resolvedIDs, fid)
		case !firstDelivery && f.State == "unchanged":
			// Carried over: referenced by id, not re-posted as a fresh inline
			// comment (#1093 case 8) - still anchored so GitHub keeps it live.
			carriedIDs = append(carriedIDs, fid)
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint,
				Body: fmt.Sprintf("(carried over, unchanged since a previous review - %s) %s: %s", fid, f.Title, f.Rationale)})
		default:
			newIDs = append(newIDs, fid)
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint, Body: f.Title + ": " + f.Rationale})
		}
	}

	var b strings.Builder
	b.WriteString(rec.Summary)
	if len(carriedIDs) > 0 {
		sort.Strings(carriedIDs)
		b.WriteString("\n\nUnchanged from a previous review: " + strings.Join(carriedIDs, ", "))
	}
	if len(resolvedIDs) > 0 {
		sort.Strings(resolvedIDs)
		b.WriteString("\n\nResolved since a previous review: " + strings.Join(resolvedIDs, ", "))
	}
	if len(rec.Clean) > 0 {
		b.WriteString("\n\nCLEAN:\n")
		for _, p := range rec.Clean {
			b.WriteString("- " + p + "\n")
		}
	}
	for _, d := range rec.Dismissed {
		b.WriteString(fmt.Sprintf("\nDismissed: %s:%d: %s", d.Path, d.Line, d.Note))
	}

	slog.Debug("rendering review from code_review artifact", "component", "vetting", "node", nodeID,
		"id", id, "revision", rev, "new", len(newIDs), "carried", len(carriedIDs), "resolved", len(resolvedIDs))

	return StagedDelivery{Kind: "review", Event: rec.Verdict, Body: strings.TrimSpace(b.String()), Comments: comments}, true
}

// renderPRBodyFromArtifact loads the latest pr_body blob and overlays it
// onto the worker's staged PR item (branch/omitted-flag bookkeeping the
// worker already set stays; only Title/Body come from the record). false
// when no pr_body record exists yet (no writer produces this kind as of
// #1093 - #1095 scope; this stays a no-op fallback until one does).
func renderPRBodyFromArtifact(ctx context.Context, cfg Config, nodeID string, staged StagedDelivery) (StagedDelivery, bool) {
	c := recordClient(cfg)
	if c == nil {
		return StagedDelivery{}, false
	}
	id, err := recordstore.IdentityFor(kindPRBody, nil, documentHint(cfg.ChatID))
	if err != nil {
		return StagedDelivery{}, false
	}
	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok {
		return StagedDelivery{}, false
	}
	staged.Body = string(raw)
	return staged, true
}
