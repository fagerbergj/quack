package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
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
