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
// node runs of the high-traffic agents into Langfuse dataset items for `quack experiment run`.
func newDatasetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dataset",
		Short: "Export recorded node runs to a Langfuse dataset",
	}
	c.AddCommand(newDatasetExportCmd())
	return c
}

func newDatasetExportCmd() *cobra.Command {
	var opts cli.ExportOpts
	var since string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export gated code-reviewer, synthesizer, web-researcher and trend-scout runs to a Langfuse dataset, idempotently",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDatasetExport(cmd, opts, since)
		},
	}
	c.Flags().StringVar(&opts.ChatID, "chat", "", "export a single chat by id")
	c.Flags().StringVar(&opts.Repo, "repo", "", "export every chat for owner/repo")
	c.Flags().StringVar(&since, "since", "", "only chats updated at or after this date (RFC3339 or YYYY-MM-DD); alone, every chat since then; with --chat, excludes it entirely if it's older")
	c.Flags().StringVar(&opts.Dataset, "dataset", "", "Langfuse dataset name (created if it doesn't exist)")
	c.Flags().StringVar(&opts.Agent, "agent", "", "export only this agent's runs")
	c.Flags().IntVar(&opts.Limit, "limit", 0, "stop after exporting this many items (0 = no limit)")
	_ = c.MarkFlagRequired("dataset")
	return c
}

func runDatasetExport(cmd *cobra.Command, opts cli.ExportOpts, since string) error {
	if opts.ChatID == "" && opts.Repo == "" && since == "" {
		return fmt.Errorf("dataset export: one of --chat, --repo or --since is required")
	}
	if since != "" {
		t, err := parseDateFlag(since)
		if err != nil {
			return fmt.Errorf("dataset export: --since: %w", err)
		}
		opts.Since = t
	}

	export, err := datasetExporter(cmd, opts)
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

	items, excludedBySince, err := export(lf)
	if err != nil {
		// Item ids are deterministic, so a re-run converges. Cobra prints err itself.
		fmt.Fprintf(cmd.ErrOrStderr(), "%d item(s) exported before failure\n", len(items))
		return err
	}
	if excludedBySince {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "chat %s excluded by --since\n", opts.ChatID)
		return err
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), cli.FormatExportSummary(items))
	return err
}

// datasetExporter reads from --server's API when it is set, else the local config's stores; it never
// falls back to the active registered server, so a bare export stays local.
func datasetExporter(cmd *cobra.Command, opts cli.ExportOpts) (func(*langfuse.Client) ([]cli.ExportItem, bool, error), error) {
	if server, _ := cmd.Flags().GetString("server"); server != "" {
		c, err := cli.NewClient(cmd.Context(), server)
		if err != nil {
			return nil, err
		}
		return func(lf *langfuse.Client) ([]cli.ExportItem, bool, error) {
			return cli.RunRemoteDatasetExport(cmd.Context(), c, lf, opts)
		}, nil
	}
	ls, st, artifacts, err := openLedgerAndStores(cmd, false)
	if err != nil {
		return nil, err
	}
	opts.Artifacts = artifacts
	return func(lf *langfuse.Client) ([]cli.ExportItem, bool, error) {
		return cli.RunDatasetExport(cmd.Context(), ls, st, lf, opts)
	}, nil
}

// parseDateFlag accepts either RFC3339 or a bare YYYY-MM-DD date (midnight UTC).
func parseDateFlag(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// langfuseClientFromConfig is the client for datasets and experiment runs: experiments.store's
// credentials, else prompts.store's. Prompt pins resolve separately, from prompts.store.
func langfuseClientFromConfig(cfg *config.Config) (*langfuse.Client, error) {
	name := cfg.ExperimentsStore()
	sc, ok := cfg.Store(name)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("langfuse is not configured: experiments.store (else prompts.store) %q is not a langfuse store (needs stores.<name> kind: langfuse with url, public_key, secret_key)", name)
	}
	return langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), nil
}
