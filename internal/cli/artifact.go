package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// RunArtifactList is `quack chat artifact list <chat-id>`: every artifact
// visible to the chat, with its latest revision's size and mime type, or raw
// JSON (full revision history) with --json.
func RunArtifactList(ctx context.Context, out io.Writer, server, chatID string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	artifacts, err := c.ListChatArtifacts(ctx, chatID)
	if err != nil {
		return notFoundAs(err, chatID)
	}
	if asJSON {
		return writeJSON(out, artifacts)
	}
	if len(artifacts) == 0 {
		fmt.Fprintln(out, "No artifacts for this chat.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREVISIONS\tLATEST SIZE\tMIME TYPE")
	for _, a := range artifacts {
		if len(a.Revisions) == 0 {
			fmt.Fprintf(tw, "%s\t0\t-\t-\n", a.Name)
			continue
		}
		latest := a.Revisions[len(a.Revisions)-1]
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", a.Name, len(a.Revisions), humanSize(latest.Size), latest.MimeType)
	}
	return tw.Flush()
}

// RunArtifactDownload is `quack chat artifact download <chat-id> <name> [--revision N] [-o file]`:
// downloads one revision's bytes to outFile (default the artifact's own
// name) and prints the written path.
func RunArtifactDownload(ctx context.Context, out io.Writer, server, chatID, name string, revision int, outFile string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if outFile == "" {
		outFile = name
	}
	body, err := c.FetchArtifact(ctx, chatID, name, revision)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("no artifact %q (or that revision) on chat %s", name, chatID)
		}
		return err
	}
	if err := os.WriteFile(outFile, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outFile, err)
	}
	fmt.Fprintln(out, outFile)
	return nil
}
