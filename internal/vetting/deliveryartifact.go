// deliveryartifact.go: renders a round's delivery from the durable code_review/finding/pr_body records instead of the worker's own staged
// text (#1093, P6/P10 of epic #1090). commitDelivery calls this on every final round, passed or failed - a code_review/document revision is written
// every round, so the posted content and the recorded delivered_revision are always the same thing, even on a gate-fail draft delivery (design V4 §4.5).
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// artifactRenderedDelivery replaces the "review" and "pr" staged items with artifact-backed renders where a record exists, leaving every other staged
// item (and either one, on any render failure) exactly as the worker staged it. staged is mutated in place and returned; fromStaged is true when any
// item fell back to the worker's own staged text (finding 2: the caller must never record such a delivery as artifact-backed). ponytail: all-or-nothing across items; fine while reviewers stage only "review" and implementers only "pr" - needs per-item scoping once #1095's pr_body writer lands alongside a rendered review in the same delivery.
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

// highlightBody composes a finding's Highlights-table/verdict-count body:
// its title as-is when the title itself already carries a Conventional-
// Comments label, otherwise the finding's own Severity field prepended as one - a write_finding-native finding carries its label in Severity, not embedded in Title's text, so without this a blocking finding written that way would show no count and never make the Highlights table.
func highlightBody(f FindingRecord) string {
	if label, _ := commentLabel(f.Title); label != "" {
		return f.Title
	}
	sev := strings.ToLower(strings.TrimSpace(f.Severity))
	for _, l := range reviewLabelOrder {
		if sev == l {
			return sev + ": " + f.Title
		}
	}
	return f.Title
}

// renderReviewFromArtifact loads the latest code_review record (called on
// every final round, whether it passed or failed - see commitDelivery) and its findings, and renders the same StagedDelivery{Kind: "review", ...}
// shape stage_review/the answer-tail parser would have produced. false when no code_review record exists yet for this subject (fresh chat, or a non-episodic reviewer config) - the caller falls back to staged text.
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
	var priorHeadSHA string
	if !firstDelivery {
		if prior, ok := latestDeliveryRecord(ctx, cfg, id); ok {
			priorHeadSHA = prior.HeadSHA
		}
	}

	var comments []ReviewComment   // for GitHub's inline posting
	var highlights []ReviewComment // Body = f.Title (keeps the label prefix), for the verdict-line counts + Highlights table
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
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid,
				Body: fmt.Sprintf("(carried over, unchanged since a previous review - %s) %s: %s", fid, f.Title, f.Rationale)})
			highlights = append(highlights, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Body: highlightBody(f)})
		default:
			newIDs = append(newIDs, fid)
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Body: f.Title + ": " + f.Rationale})
			highlights = append(highlights, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Body: highlightBody(f)})
		}
	}

	in := reviewOverviewInput{
		Verdict:  rec.Verdict,
		Takeaway: rec.Takeaway,
		Verified: rec.Verified,
		Notes:    rec.Notes,
		Comments: highlights,
	}
	// Legacy read path: a pre-migration record has Summary but never
	// Takeaway/Verified/Notes - render it under Notes, truncated, so old
	// history still displays (renderReviewOverview's LegacySummary).
	if strings.TrimSpace(rec.Takeaway) == "" && len(rec.Verified) == 0 && len(rec.Notes) == 0 {
		in.LegacySummary = rec.Summary
	}
	if !firstDelivery {
		in.SinceKnown = true
		in.Resolved = len(resolvedIDs)
		in.Open = len(highlights)
		in.Dismissed = rec.Dismissed
	}
	if scope := resolveReviewScope(cfg); scope.ok {
		in.ScopeKnown = true
		in.HeadSHA = scope.head
		in.FileCount = scope.fileCount
		in.FirstReview = firstDelivery
		if !firstDelivery {
			in.PriorHeadSHA = priorHeadSHA
			if n, ok := scope.commitsSince(priorHeadSHA); ok {
				in.CommitsSinceKnown = true
				in.CommitsSince = n
			}
		}
	}
	body := renderReviewOverview(in)

	slog.Debug("rendering review from code_review artifact", "component", "vetting", "node", nodeID,
		"id", id, "revision", rev, "new", len(newIDs), "carried", len(carriedIDs), "resolved", len(resolvedIDs))

	return StagedDelivery{Kind: "review", Event: rec.Verdict, Body: body, Comments: comments}, true
}

// renderPRBodyFromArtifact loads the latest pr_body blob and overlays it
// onto the worker's staged PR item (branch/omitted-flag bookkeeping the
// worker already set stays; only Title/Body come from the record). false when no pr_body record exists yet (no writer produces this kind as of #1093 - #1095 scope; this stays a no-op fallback until one does).
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
