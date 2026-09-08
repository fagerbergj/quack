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
// server's configured memory stores, or raw JSON with --json.
func RunMemoryList(ctx context.Context, out io.Writer, server, bucket, q string, limit int, includeInvalidated, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	list, err := c.ListMemories(ctx, bucket, q, limit, includeInvalidated)
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
// No single-memory GET endpoint exists yet - this pages through
// include_invalidated=true listings looking for the id, fine at memory's
// documented scale (hundreds-thousands).
func RunMemoryShow(ctx context.Context, out io.Writer, server, id string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	m, err := findMemory(ctx, c, id)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("memory %s not found", id)
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

// findMemory pages through every bucket's listing (include_invalidated so a
// forgotten memory is still showable) looking for id.
func findMemory(ctx context.Context, c *Client, id string) (*schema.Memory, error) {
	var pageToken string
	for {
		list, err := c.ListMemoriesPage(ctx, "", pageToken, memoryShowPageSize, true)
		if err != nil {
			return nil, err
		}
		for i := range list.Memories {
			if list.Memories[i].Id == id {
				return &list.Memories[i], nil
			}
		}
		if list.NextPageToken == nil || *list.NextPageToken == "" {
			return nil, nil
		}
		pageToken = *list.NextPageToken
	}
}

// memoryShowPageSize bounds each page findMemory fetches while scanning for one id.
const memoryShowPageSize = 200

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

// RunMemorySweep is `quack memory sweep [--dry-run]`: runs the forgetting-rule
// sweep on demand against every store the server has configured, printing
// per-rule counts (and, dry-run, up to 5 example memories per rule).
func RunMemorySweep(ctx context.Context, out io.Writer, server string, dryRun, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	res, err := c.SweepMemories(ctx, dryRun)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, res)
	}
	if len(res.Stores) == 0 {
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
	if res.Errors != nil {
		for _, e := range *res.Errors {
			fmt.Fprintf(out, "store %s failed: %s\n", e.Store, e.Message)
		}
	}
	if res.DryRun {
		fmt.Fprintln(out, "(dry run: nothing was invalidated)")
	}
	if res.Errors != nil && len(*res.Errors) > 0 {
		return fmt.Errorf("%d memory store(s) failed to sweep", len(*res.Errors))
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
