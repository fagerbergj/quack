package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
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
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return err
	}
	lf, err := langfuseGenClientFromConfig(cfg)
	if err != nil {
		return err
	}

	items, err := cli.RunDatasetExport(cmd.Context(), ls, st, lf, cli.ExportOpts{
		ChatID: chatID, Repo: repo, Since: sinceT, Dataset: dataset, Limit: limit,
	})
	if err != nil {
		// Export is idempotent (item ids are deterministic), so a re-run after
		// this partial failure converges rather than duplicating.
		fmt.Fprintf(cmd.ErrOrStderr(), "%d item(s) exported before failure: %v\n", len(items), err)
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

// langfuseGenClientFromConfig builds the generated dataset/score client from
// cfg.Prompts.Store's langfuse credentials - the same store `prompts:` resolves against.
func langfuseGenClientFromConfig(cfg *config.Config) (*langfusegen.ClientWithResponses, error) {
	sc, ok := cfg.Store(cfg.Prompts.Store)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("dataset/experiment: prompts.store %q is not a configured langfuse store", cfg.Prompts.Store)
	}
	return langfuse.NewGenClient(sc.URL, sc.PublicKey, sc.SecretKey)
}
