package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/langfuse"
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
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
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
	pinSrc, err := pinnedPromptSource(ctx, cfg, agent, prompt)
	if err != nil {
		return err
	}
	base, stop, err := serve.InProcessWithPromptSource(ctx, cfg, pinSrc)
	if err != nil {
		return err
	}
	defer func() { _ = stop() }()

	runner := cli.NewLiveItemRunner(base, st, agent)
	results, runErr := cli.RunExperiment(ctx, cmd.ErrOrStderr(), runner, lf, cli.ExperimentOpts{
		Dataset: dataset, Agent: agent, Prompt: prompt, RunName: runName, Limit: limit,
	})
	// Report whatever completed even on a hard mid-run error (runErr != nil):
	// RunExperiment already printed the partial text summary to stderr, so
	// --as-json's structured output isn't left as the only form that drops it.
	if asJSON {
		if err := cli.WriteJSON(cmd.OutOrStdout(), results); err != nil {
			return err
		}
	} else if runErr == nil {
		if _, err := fmt.Fprint(cmd.OutOrStdout(), cli.FormatExperimentSummary(results)); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	if n := errorCount(results); n > 0 {
		return fmt.Errorf("experiment run: %d item(s) errored", n)
	}
	return nil
}

func errorCount(results []cli.ExperimentResult) int {
	n := 0
	for _, r := range results {
		if r.Error != "" {
			n++
		}
	}
	return n
}

// pinnedPromptSource turns --prompt into a Source resolved eagerly, so a bad
// name/version fails the command instead of falling back to the static prompt.
// nil when no pin was asked for.
func pinnedPromptSource(ctx context.Context, cfg *config.Config, agent, prompt string) (artifactsrc.Source, error) {
	if prompt == "" {
		return nil, nil
	}
	name, version, err := langfuse.ParsePin(prompt)
	if err != nil {
		return nil, err
	}
	if want := "system/" + agent; name != want {
		return nil, fmt.Errorf("--prompt %s: name must be %s to match --agent %s", prompt, want, agent)
	}
	sc, ok := cfg.Store(cfg.Prompts.Store)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("--prompt needs prompts.store to name a langfuse store")
	}
	src := &langfuse.PinnedSource{Client: langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), Pins: map[string]int{name: version}}
	if err := src.ResolveNow(ctx, name); err != nil {
		return nil, err
	}
	return src, nil
}
