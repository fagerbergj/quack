package main

import (
	"fmt"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/langfuse"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/serve"
)

func nowStamp() string { return time.Now().UTC().Format("20060102-150405") }

// newExperimentCmd: `quack experiment run` (P5 of #1418/#1424) - runs an agent against a
// Langfuse dataset outside any live GitHub event, reporting each item as a dataset run item.
func newExperimentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "experiment",
		Short: "Run an agent against a Langfuse dataset",
	}
	c.AddCommand(newExperimentRunCmd())
	return c
}

func newExperimentRunCmd() *cobra.Command {
	var dataset, agent, prompt, runName string
	var limit int
	var asJSON bool
	c := &cobra.Command{
		Use:   "run",
		Short: "Run --agent's node against every item in --dataset, reporting a Langfuse dataset run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExperimentRun(cmd, dataset, agent, prompt, runName, limit, asJSON)
		},
	}
	c.Flags().StringVar(&dataset, "dataset", "", "Langfuse dataset name")
	c.Flags().StringVar(&agent, "agent", "", "agent name to run each item against")
	c.Flags().StringVar(&prompt, "prompt", "", "system/<agent>@N - pin the run's prompt to that exact Langfuse version")
	c.Flags().StringVar(&runName, "run-name", "", "Langfuse dataset run name (default: <agent>-<timestamp>)")
	c.Flags().IntVar(&limit, "limit", 0, "run at most this many items (0 = no limit)")
	asJSONFlag(c, &asJSON)
	_ = c.MarkFlagRequired("dataset")
	_ = c.MarkFlagRequired("agent")
	return c
}

func runExperimentRun(cmd *cobra.Command, dataset, agent, prompt, runName string, limit int, asJSON bool) error {
	if runName == "" {
		runName = agent + "-" + nowStamp()
	}
	cfgPath := defaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("no %s found - `quack experiment run` needs a LOCAL quack.yaml (run `quack init` first)", cfgPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	lf, err := langfuseGenClientFromConfig(cfg)
	if err != nil {
		return err
	}

	_, st, _, err := openLedgerAndStores()
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	pinSrc, err := pinnedPromptSource(cfg, prompt)
	if err != nil {
		return err
	}
	base, stop, err := serve.InProcessWithPromptSource(ctx, cfg, pinSrc)
	if err != nil {
		return err
	}
	defer func() { _ = stop() }()

	runner := cli.NewLiveItemRunner(base, st, agent)
	results, err := cli.RunExperiment(ctx, cmd.ErrOrStderr(), runner, lf, cli.ExperimentOpts{
		Dataset: dataset, Agent: agent, Prompt: prompt, RunName: runName, Limit: limit,
	})
	if err != nil {
		return err
	}
	if asJSON {
		return cli.WriteJSON(cmd.OutOrStdout(), results)
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), cli.FormatExperimentSummary(results))
	return err
}

// pinnedPromptSource turns --prompt into a Source that serves exactly that version; nil
// when no pin was asked for, so the configured store resolves as in a live run.
func pinnedPromptSource(cfg *config.Config, prompt string) (artifactsrc.Source, error) {
	if prompt == "" {
		return nil, nil
	}
	name, version, err := langfuse.ParsePin(prompt)
	if err != nil {
		return nil, err
	}
	sc, ok := cfg.Store(cfg.Prompts.Store)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("--prompt needs prompts.store to name a langfuse store")
	}
	return &langfuse.PinnedSource{Client: langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), Pins: map[string]int{name: version}}, nil
}
