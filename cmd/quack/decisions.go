package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
)

// newDecisionsCmd: `quack decisions report|export` - compare the decision model against
// quack's own logic from the ledger's decision entries.
func newDecisionsCmd() *cobra.Command {
	c := &cobra.Command{Use: "decisions", Short: "Compare decision-model answers against quack's own decisions"}
	c.AddCommand(newDecisionsReportCmd(), newDecisionsExportCmd())
	return c
}

type decisionFlags struct {
	since string
	point string
	chats []string
}

func (d *decisionFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&d.since, "since", "", "only decisions newer than this: a duration (7d, 36h), RFC3339 or YYYY-MM-DD")
	c.Flags().StringVar(&d.point, "point", "", "only this decision point id")
	c.Flags().StringArrayVar(&d.chats, "chat", nil, "only this chat id (repeatable)")
}

func (d *decisionFlags) filter() (cli.DecisionFilter, error) {
	f := cli.DecisionFilter{Point: d.point, Chats: d.chats}
	if d.since == "" {
		return f, nil
	}
	t, err := parseSince(d.since, time.Now())
	if err != nil {
		return f, fmt.Errorf("--since: %w", err)
	}
	f.Since = t
	return f, nil
}

// parseSince accepts a relative window ("7d" or any time.ParseDuration string) or an absolute date.
func parseSince(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil {
			if days <= 0 {
				return time.Time{}, errNonPositiveWindow(s)
			}
			return now.AddDate(0, 0, -days), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, errNonPositiveWindow(s)
		}
		return now.Add(-d), nil
	}
	return parseDateFlag(s)
}

func errNonPositiveWindow(s string) error {
	return fmt.Errorf("relative window must be positive (got %s)", s)
}

func newDecisionsReportCmd() *cobra.Command {
	var fl decisionFlags
	var asJSON bool
	c := &cobra.Command{
		Use:   "report",
		Short: "Per decision point: coverage, agreement with quack's baseline, latency, confusion",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := fl.filter()
			if err != nil {
				return err
			}
			return withTarget(cmd, func(t string) error {
				return cli.RunDecisionsReport(cmd.Context(), cmd.OutOrStdout(), t, f, asJSON)
			})
		},
	}
	fl.bind(c)
	asJSONFlag(c, &asJSON)
	return c
}

func newDecisionsExportCmd() *cobra.Command {
	var fl decisionFlags
	var toLangfuse bool
	c := &cobra.Command{
		Use:   "export --langfuse",
		Short: "Upsert decisions as one Langfuse dataset per point, with a run per handler@version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !toLangfuse {
				return fmt.Errorf("decisions export: --langfuse is required (the only destination)")
			}
			f, err := fl.filter()
			if err != nil {
				return err
			}
			lf, ing, err := langfuseClientsFromLocalConfig()
			if err != nil {
				return fmt.Errorf("decisions export: %w", err)
			}
			return withTarget(cmd, func(t string) error {
				return cli.RunDecisionsExport(cmd.Context(), cmd.OutOrStdout(), t, f, lf, ing)
			})
		},
	}
	fl.bind(c)
	c.Flags().BoolVar(&toLangfuse, "langfuse", false, "write to the langfuse store named by prompts.store in quack.yaml")
	return c
}

// langfuseClientsFromLocalConfig reads credentials from the local quack.yaml, as `quack dataset export` does.
func langfuseClientsFromLocalConfig() (*langfusegen.ClientWithResponses, *langfuse.Client, error) {
	cfg, err := config.LoadDeferringAgentCompleteness(defaultConfigPath())
	if err != nil {
		return nil, nil, err
	}
	lf, err := langfuseGenClientFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	sc, _ := cfg.Store(cfg.Prompts.Store)
	return lf, langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), nil
}
