package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
)

// RunMemoryRescope is `quack memory rescope`: move role:* memories whose
// provenance chat has a GitHub origin into their repo:* bucket (#1262). Dry
// run by default; --apply actually writes the change.
func RunMemoryRescope(ctx context.Context, out io.Writer, server string, apply, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	report, err := c.RescopeMemories(ctx, apply)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, report)
	}
	verb := "would move"
	if report.Applied {
		verb = "moved"
	}
	if len(report.Repos) == 0 {
		fmt.Fprintln(out, "No role:* memories resolve to a GitHub-origin chat.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "REPO\t%s\tEXAMPLE\n", "COUNT")
	for _, r := range report.Repos {
		example := ""
		if r.Examples != nil && len(*r.Examples) > 0 {
			example = truncateLine((*r.Examples)[0], 60)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Repo, r.Count, example)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	total := 0
	for _, r := range report.Repos {
		total += r.Count
	}
	fmt.Fprintf(out, "\n%s %d memories across %d repos.\n", verb, total, len(report.Repos))
	return nil
}
