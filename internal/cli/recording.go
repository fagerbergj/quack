package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// RunRecordingList is `quack recording list`: sessions the replay ledger has
// an entry for (id, size, last-modified), or raw JSON with --json.
func RunRecordingList(ctx context.Context, out io.Writer, server string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	recs, err := c.ListRecordings(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("recording is not enabled on this server")
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
	fmt.Fprintln(tw, "CHAT ID\tSIZE\tMODIFIED")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.ChatId, humanSize(r.SizeBytes), r.ModifiedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

// RunRecordingExport is `quack recording export <chat-id> [-o file]`:
// downloads the chat's replay-ledger bundle to outFile (default
// "<chat-id>.zip") and prints the written path. A partial file from a
// mid-download failure is removed rather than left around half-written.
func RunRecordingExport(ctx context.Context, out io.Writer, server, chatID, outFile string) error {
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
			return fmt.Errorf("no recording for chat %s (never recorded, already GC'd by retention, or recording disabled)", chatID)
		}
		return err
	}
	if err := os.WriteFile(outFile, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outFile, err)
	}
	fmt.Fprintln(out, outFile)
	return nil
}

// humanSize renders n bytes as a short human-readable size (B/KB/MB/GB) -
// recording bundles span a few KB (a short chat) to tens of MB (a long
// multi-node run with a clone snapshot).
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
