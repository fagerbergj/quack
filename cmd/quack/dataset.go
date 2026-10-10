package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/langfuse"
)

// newDatasetCmd: `quack dataset export` (P5 of #1418/#1424) - turns recorded
// code-reviewer/synthesizer node runs into Langfuse dataset items for `quack experiment run`.
func newDatasetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dataset",
		Short: "Export recorded node runs to a Langfuse dataset",
	}
	c.AddCommand(newDatasetExportCmd())
	return c
}

func newDatasetExportCmd() *cobra.Command {
	var chatID, repo, since, dataset string
	var limit int
	c := &cobra.Command{
		Use:   "export",
		Short: "Export gated code-reviewer/synthesizer runs to a Langfuse dataset, idempotently",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDatasetExport(cmd, chatID, repo, since, dataset, limit)
		},
	}
	c.Flags().StringVar(&chatID, "chat", "", "export a single chat by id")
	c.Flags().StringVar(&repo, "repo", "", "export every chat for owner/repo")
	c.Flags().StringVar(&since, "since", "", "only chats updated at or after this date (RFC3339 or YYYY-MM-DD); with --chat, excludes it entirely if it's older")
	c.Flags().StringVar(&dataset, "dataset", "", "Langfuse dataset name (created if it doesn't exist)")
	c.Flags().IntVar(&limit, "limit", 0, "stop after exporting this many items (0 = no limit)")
	_ = c.MarkFlagRequired("dataset")
	return c
}

func runDatasetExport(cmd *cobra.Command, chatID, repo, since, dataset string, limit int) error {
	if chatID == "" && repo == "" {
		return fmt.Errorf("dataset export: one of --chat or --repo is required")
	}
	var sinceT time.Time
	if since != "" {
		t, err := parseDateFlag(since)
		if err != nil {
			return fmt.Errorf("dataset export: --since: %w", err)
		}
		sinceT = t
	}

	ls, st, _, err := openLedgerAndStores()
	if err != nil {
		return err
	}
	// Defer bundle checks: export reads only langfuse creds, and plugin-seeded bundles aren't in the file.
	cfg, err := config.LoadDeferringAgentCompleteness(defaultConfigPath())
	if err != nil {
		return err
	}
	lf, err := langfuseClientFromConfig(cfg)
	if err != nil {
		return err
	}

	items, excludedBySince, err := cli.RunDatasetExport(cmd.Context(), ls, st, lf, cli.ExportOpts{
		ChatID: chatID, Repo: repo, Since: sinceT, Dataset: dataset, Limit: limit,
	})
	if err != nil {
		// Item ids are deterministic, so a re-run converges. Cobra prints err itself.
		fmt.Fprintf(cmd.ErrOrStderr(), "%d item(s) exported before failure\n", len(items))
		return err
	}
	if excludedBySince {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "chat %s excluded by --since\n", chatID)
		return err
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), cli.FormatExportSummary(items))
	return err
}

// parseDateFlag accepts either RFC3339 or a bare YYYY-MM-DD date (midnight UTC).
func parseDateFlag(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// langfuseClientFromConfig uses cfg.Prompts.Store's langfuse credentials, the store `prompts:` resolves against.
func langfuseClientFromConfig(cfg *config.Config) (*langfuse.Client, error) {
	sc, ok := cfg.Store(cfg.Prompts.Store)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("langfuse is not configured: prompts.store %q is not a langfuse store (needs stores.<name> kind: langfuse with url, public_key, secret_key)", cfg.Prompts.Store)
	}
	return langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), nil
}
