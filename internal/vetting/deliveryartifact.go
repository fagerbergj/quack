// deliveryartifact.go: renders delivery from the durable code_review/finding/pr_body records, so posted
// content and the recorded delivered_revision always match, even on a gate-fail draft.
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// artifactRenderedDelivery swaps "review"/"pr" for artifact renders where a record exists; fromStaged: one fell back.
// ponytail: all-or-nothing across items; per-item scoping once a review and pr_body ship in one delivery.
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

// highlightBody: the title as-is when it carries a Conventional-Comments label, else Severity
// prepended, since write_finding keeps the label there and it would otherwise go uncounted.
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

// renderReviewFromArtifact renders the latest code_review record and its findings in stage_review's
// shape; false when none exists yet, and the caller falls back to staged text.
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

	comments, highlights, newIDs, carriedIDs, resolvedIDs := classifyReviewFindings(ctx, c, firstDelivery, rec.FindingIDs)
	in := reviewOverviewFromArtifact(cfg, firstDelivery, priorHeadSHA, rec, highlights, len(resolvedIDs))
	body := renderReviewOverview(in)

	slog.Debug("rendering review from code_review artifact", "component", "vetting", "node", nodeID,
		"id", id, "revision", rev, "new", len(newIDs), "carried", len(carriedIDs), "resolved", len(resolvedIDs))

	return StagedDelivery{Kind: "review", Event: rec.Verdict, Body: body, Comments: comments}, true
}

// classifyReviewFindings: split this round's findings into new / carried-over /
// resolved, with the GitHub inline comments and the highlight rows.
func classifyReviewFindings(ctx context.Context, c *recordstore.Client, firstDelivery bool, findingIDs []string) (comments, highlights []ReviewComment, newIDs, carriedIDs, resolvedIDs []string) {
	for _, fid := range findingIDs {
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
			// Carried over: referenced by id, not re-posted, but still anchored so GitHub keeps it live.
			carriedIDs = append(carriedIDs, fid)
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Severity: f.Severity,
				Body: fmt.Sprintf("(carried over, unchanged since a previous review - %s) %s: %s", fid, f.Title, f.Rationale)})
			highlights = append(highlights, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Body: highlightBody(f)})
		default:
			newIDs = append(newIDs, fid)
			comments = append(comments, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Severity: f.Severity, Body: f.Title + ": " + f.Rationale})
			highlights = append(highlights, ReviewComment{Path: f.Path, Line: f.LineHint, FindingID: fid, Body: highlightBody(f)})
		}
	}
	return
}

// reviewOverviewFromArtifact: the overview input for an artifact-rendered review
// (legacy Summary fallback, re-review deltas, git scope).
func reviewOverviewFromArtifact(cfg Config, firstDelivery bool, priorHeadSHA string, rec CodeReviewRecord, highlights []ReviewComment, resolvedCount int) reviewOverviewInput {
	in := reviewOverviewInput{
		Verdict:  rec.Verdict,
		Takeaway: rec.Takeaway,
		Verified: rec.Verified,
		Notes:    rec.Notes,
		Comments: highlights,
	}
	// Legacy read path: a pre-migration record has Summary but never
	// Takeaway/Verified/Notes - render it under Notes, truncated (LegacySummary).
	if strings.TrimSpace(rec.Takeaway) == "" && len(rec.Verified) == 0 && len(rec.Notes) == 0 {
		in.LegacySummary = rec.Summary
	}
	if !firstDelivery {
		in.SinceKnown = true
		in.Resolved = resolvedCount
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
	return in
}

// renderPRBodyFromArtifact overlays the latest pr_body's Title/Body onto the staged PR item;
// false when no pr_body record exists (no writer produces one yet).
func renderPRBodyFromArtifact(ctx context.Context, cfg Config, nodeID string, staged StagedDelivery) (StagedDelivery, bool) {
	c := recordClient(cfg)
	if c == nil {
		return StagedDelivery{}, false
	}
	id, err := recordstore.IdentityFor(kindPRBody, nil, DocumentHint(cfg.ChatID))
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
