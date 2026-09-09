package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fagerbergj/quack/internal/schema"
)

// RunMemoryList is `quack memory list`: browse or (with q) search the
// server's configured memory stores, or raw JSON with --json. limit<=0
// (the default) auto-pages through the whole listing - see Client.ListMemories.
func RunMemoryList(ctx context.Context, out io.Writer, server, bucket, q, tier, sort string, limit int, includeInvalidated, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	list, err := c.ListMemories(ctx, bucket, q, tier, sort, limit, includeInvalidated)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, list)
	}
	if len(list.Memories) == 0 {
		fmt.Fprintln(out, "No memories match.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tBUCKET\tSTATUS\tCONTENT")
	for _, m := range list.Memories {
		status := "unverified"
		if m.Status != nil {
			status = string(*m.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Id, m.Bucket, status, truncateLine(m.Content, 80))
	}
	return tw.Flush()
}

// RunMemoryShow is `quack memory show <memory-id>`: prints one memory's full
// detail, including votes/tier/last-recalled (epic #1255 P1 observability).
// A direct per-id GET, not a full-store page scan.
func RunMemoryShow(ctx context.Context, out io.Writer, server, id string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	m, err := c.GetMemory(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("memory %s not found", id)
		}
		return err
	}
	if asJSON {
		return writeJSON(out, m)
	}
	tier := "unverified"
	if m.Tier != nil {
		tier = string(*m.Tier)
	}
	status := "unverified"
	if m.Status != nil {
		status = string(*m.Status)
	}
	fmt.Fprintf(out, "id:       %s\n", m.Id)
	fmt.Fprintf(out, "bucket:   %s\n", m.Bucket)
	fmt.Fprintf(out, "status:   %s\n", status)
	fmt.Fprintf(out, "tier:     %s\n", tier)
	fmt.Fprintf(out, "votes:    +%d / -%d (score %d)\n", intOr(m.Upvotes), intOr(m.Downvotes), intOr(m.VoteScore))
	fmt.Fprintf(out, "recalls:  %d\n", intOr(m.Recalls))
	if m.LastUpvotedAt != nil {
		fmt.Fprintf(out, "last upvoted:  %s\n", m.LastUpvotedAt.Format(time.RFC3339))
	}
	if m.LastRecalledAt != nil {
		fmt.Fprintf(out, "last recalled: %s\n", m.LastRecalledAt.Format(time.RFC3339))
	}
	if m.InvalidationReason != nil {
		fmt.Fprintf(out, "invalidation reason: %s\n", *m.InvalidationReason)
	}
	fmt.Fprintf(out, "content:\n%s\n", m.Content)
	return nil
}

func intOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// RunMemoryForget is `quack memory forget <memory-id> [--reason text]`:
// soft-delete (invalidate) one memory.
func RunMemoryForget(ctx context.Context, out io.Writer, server, id, reason string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.ForgetMemory(ctx, id, reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("memory %s not found", id)
		}
		return err
	}
	fmt.Fprintf(out, "invalidated %s\n", id)
	return nil
}

// RunMemorySweep is `quack memory sweep [--dry-run] [--dedupe [--apply]]`:
// runs the forgetting-rule sweep (default) or, with --dedupe, the per-bucket
// similarity dedupe sweep (issue #1269) on demand against every store the
// server has configured. Without --apply, --dedupe only clusters and
// reports examples - no LLM call, nothing written.
func RunMemorySweep(ctx context.Context, out io.Writer, server string, dryRun, dedupe, apply, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	res, err := c.SweepMemories(ctx, dryRun, dedupe, apply)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, res)
	}
	hasErrors := res.Errors != nil && len(*res.Errors) > 0
	if dedupe {
		return printDedupeReport(out, res, hasErrors)
	}
	if len(res.Stores) == 0 && !hasErrors {
		fmt.Fprintln(out, "No memory stores configured.")
		return nil
	}
	for _, s := range res.Stores {
		fmt.Fprintf(out, "store %s: evaluated %d, kept %d\n", s.Store, s.Evaluated, s.Kept)
		for _, r := range s.Rules {
			fmt.Fprintf(out, "  rule %d [%s -> %s]: matched %d\n", r.Index, r.When, r.Then, r.Matched)
			if r.Examples != nil {
				for _, e := range *r.Examples {
					fmt.Fprintf(out, "    - %s: %s\n", e.Id, truncateLine(e.Content, 80))
				}
			}
		}
	}
	if hasErrors {
		for _, e := range *res.Errors {
			fmt.Fprintf(out, "store %s failed: %s\n", e.Store, e.Message)
		}
	}
	if res.DryRun {
		fmt.Fprintln(out, "(dry run: nothing was invalidated)")
	}
	if hasErrors {
		return fmt.Errorf("%d memory store(s) failed to sweep", len(*res.Errors))
	}
	return nil
}

// printDedupeReport prints one --dedupe sweep's per-store cluster report.
func printDedupeReport(out io.Writer, res schema.SweepMemoriesResult, hasErrors bool) error {
	dedupe := res.Dedupe
	if dedupe == nil || len(*dedupe) == 0 {
		if !hasErrors {
			fmt.Fprintln(out, "No memory stores configured.")
		}
	}
	if dedupe != nil {
		for _, s := range *dedupe {
			fmt.Fprintf(out, "store %s: %d cluster(s), %d LLM call(s), %d op(s) applied, %d dropped\n",
				s.Store, s.NumClusters, s.LlmCalls, s.OpsApplied, s.Dropped)
			for _, c := range s.Clusters {
				fmt.Fprintf(out, "  [%s] cluster of %d\n", c.Bucket, c.Size)
				for _, e := range c.Examples {
					fmt.Fprintf(out, "    - %s: %s\n", e.Id, truncateLine(e.Content, 80))
				}
			}
		}
	}
	if hasErrors {
		for _, e := range *res.Errors {
			fmt.Fprintf(out, "store %s failed: %s\n", e.Store, e.Message)
		}
	}
	if res.DryRun {
		fmt.Fprintln(out, "(dry run: nothing was merged)")
	}
	if hasErrors {
		return fmt.Errorf("%d memory store(s) failed to sweep", len(*res.Errors))
	}
	return nil
}

// RunMemoryStats is `quack memory stats [--weeks N]`: prints the weekly
// recall precision/support-share/vote/recall table plus the current
// live/invalidated snapshot per scope (epic #1255 P5).
func RunMemoryStats(ctx context.Context, out io.Writer, server string, weeks int, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	stats, err := c.GetMemoryStats(ctx, weeks)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, stats)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WEEK\tRECALLS\tSUPPORTED\tCONTRADICTED\tNOT_RELEVANT\tPRECISION\tSUPPORT_SHARE\tMINTED\tINVALIDATED")
	for _, w := range stats.Weeks {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.2f\t%.2f\t%d\t%d\n",
			w.Week, w.Recalls, w.Supported, w.Contradicted, w.NotRelevant, w.Precision, w.SupportShare, w.Minted, w.Invalidated)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(stats.Scopes) > 0 {
		fmt.Fprintln(out)
		tw2 := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw2, "SCOPE\tLIVE\tINVALIDATED")
		for _, s := range stats.Scopes {
			fmt.Fprintf(tw2, "%s\t%d\t%d\n", s.Scope, s.Live, s.Invalidated)
		}
		return tw2.Flush()
	}
	return nil
}

// truncateLine collapses newlines to spaces and clips to n runes (with a "…"
// marker) - memory content is free text and can run to paragraphs, which
// would wreck the table's row-per-memory layout.
func truncateLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
