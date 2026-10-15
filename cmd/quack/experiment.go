package main

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/serve"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
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
	var o cli.ExperimentOpts
	var concurrency, model, judgeModel string
	var asJSON, keepWorkspaces bool
	c := &cobra.Command{
		Use:   "run",
		Short: "Run --agent's node against every item in --dataset, reporting a Langfuse dataset run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := parseConcurrency(concurrency)
			if err != nil {
				return err
			}
			o.Concurrency = n
			return runExperimentRun(cmd, o, model, judgeModel, keepWorkspaces, asJSON)
		},
	}
	c.Flags().StringVar(&o.Dataset, "dataset", "", "Langfuse dataset name")
	c.Flags().StringVar(&o.Agent, "agent", "", "agent name to run each item against")
	c.Flags().StringVar(&o.Prompt, "prompt", "", "system/<agent>@N - pin the run's prompt to that exact Langfuse version")
	c.Flags().StringVar(&o.RunName, "run-name", "", "Langfuse dataset run name (default: <agent>-<timestamp>)")
	c.Flags().IntVar(&o.Limit, "limit", 0, "run at most this many items (0 = no limit)")
	c.Flags().StringVar(&concurrency, "concurrency", "1", `run up to this many items at once; 0 or "all" runs every item at once`)
	c.Flags().StringVar(&model, "model", "", "<provider>/<model> - run --agent's worker on this models: entry instead")
	c.Flags().StringVar(&judgeModel, "judge-model", "", "<provider>/<model> - judge on this models: entry instead of gates.judge's")
	c.Flags().BoolVar(&keepWorkspaces, "keep-workspaces", false, "leave each item's clone and node dirs on disk, and skip the stale sweep (debugging)")
	asJSONFlag(c, &asJSON)
	_ = c.MarkFlagRequired("dataset")
	_ = c.MarkFlagRequired("agent")
	return c
}

func runExperimentRun(cmd *cobra.Command, o cli.ExperimentOpts, model, judgeModel string, keepWorkspaces, asJSON bool) error {
	if o.RunName == "" {
		o.RunName = o.Agent + "-" + nowStamp()
	}
	cfgPath := defaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("no %s found - `quack experiment run` needs a LOCAL quack.yaml (run `quack init` first)", cfgPath)
	}
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
	if err != nil {
		return err
	}
	lf, err := langfuseClientFromConfig(cfg)
	if err != nil {
		return err
	}

	_, st, _, err := openLedgerAndStores(cmd, true)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	rubric, err := experimentRubric(ctx, cfgPath, o.Agent)
	if err != nil {
		return err
	}
	cleanup := experimentCleanup(cmd, cfg, keepWorkspaces)
	base, stop, err := bootExperiment(ctx, cfg, o, model, judgeModel)
	if err != nil {
		return err
	}
	defer func() { _ = stop() }()

	runner := cli.NewLiveItemRunner(base, st, o.Agent, rubric)
	runner.Cleanup = cleanup
	results, runErr := cli.RunExperiment(ctx, cmd.ErrOrStderr(), runner, lf, o)
	// Emit partial results even when runErr != nil, matching the text summary RunExperiment already printed.
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

// parseConcurrency reads --concurrency: a count, or "all" (0) for every item at once.
func parseConcurrency(s string) (int, error) {
	if s == "all" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf(`--concurrency %q: want a count >= 0 or "all"`, s)
	}
	return n, nil
}

// bootExperiment boots the in-process server with --prompt's pin, the model overrides, and room to clone
// every in-flight item at once.
func bootExperiment(ctx context.Context, cfg *config.Config, o cli.ExperimentOpts, model, judgeModel string) (string, func() error, error) {
	pinSrc, err := pinnedPromptSource(ctx, cfg, o.Agent, o.Prompt)
	if err != nil {
		return "", nil, err
	}
	if err := cli.OverrideModels(cfg, o.Agent, model, judgeModel); err != nil {
		return "", nil, err
	}
	cli.EnableExperiment(cfg, o.Agent)
	dag.SetProvisionSlots(cmp.Or(o.Concurrency, math.MaxInt32)) // zero-size slots: a huge bound costs nothing
	return serve.InProcessWithPromptSource(ctx, cfg, pinSrc)
}

// staleExperimentAge: past the 2h item timeout with room to spare, so no live item in any process qualifies.
const staleExperimentAge = 6 * time.Hour

// experimentCleanup sweeps the workspaces a killed run left and returns the per-item removal; nil keeps them
// (--keep-workspaces, or no usable workspace root). Each item's clone would otherwise stay on disk for good.
func experimentCleanup(cmd *cobra.Command, cfg *config.Config, keep bool) func(chatID string) {
	if keep {
		return nil
	}
	jail, err := workspace.NewJail(cfg.Workspace.Root)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "experiment workspaces will not be cleaned up: %v\n", err)
		return nil
	}
	if n := serve.SweepExperimentWorkspaces(jail, staleExperimentAge); n > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "removed %d stale experiment workspace(s)\n", n)
	}
	return func(chatID string) {
		if err := serve.RemoveChatWorkspace(jail, chatID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "chat %s: workspace not removed: %v\n", chatID, err)
		}
	}
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

// experimentRubric loads agent's rubric scales from its own plugin-seeded config load, so a plugin agent
// resolves without seeding the cfg the in-process server boots from.
func experimentRubric(ctx context.Context, cfgPath, agent string) (vetting.RubricScaler, error) {
	_, bundle, err := agentBundle(cfgPath, agent)
	if err != nil {
		return vetting.RubricScaler{}, err
	}
	return vetting.LoadRubricScaler(ctx, nil, bundle)
}

// agentBundle is agent's bundle dir from a plugin-seeded config load, with that config.
func agentBundle(cfgPath, agent string) (*config.Config, string, error) {
	cfg, err := loadSeededConfig(cfgPath)
	if err != nil {
		return nil, "", err
	}
	ac, ok := cfg.Agents[agent]
	if !ok {
		return nil, "", fmt.Errorf("agent %s: not in quack.yaml or a locally cloned, enabled plugin", agent)
	}
	return cfg, ac.Bundle, nil
}

// pinnedPromptSource resolves --prompt eagerly so a bad pin fails the command instead of
// falling back to the static prompt; nil when no pin was asked for.
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
	client, err := promptsClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("--prompt: %w", err)
	}
	src := &langfuse.PinnedSource{Client: client, Pins: map[string]int{name: version}}
	if err := src.ResolveNow(ctx, name); err != nil {
		return nil, err
	}
	return src, nil
}

// promptsClient is prompts.store's Langfuse client: the project --prompt pins read and prompt push writes.
func promptsClient(cfg *config.Config) (*langfuse.Client, error) {
	sc, ok := cfg.Store(cfg.Prompts.Store)
	if !ok || sc.Kind != "langfuse" {
		return nil, fmt.Errorf("prompts.store must name a langfuse store")
	}
	return langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey), nil
}

// newPromptCmd: `quack prompt push <agent>` seeds an experiment's baseline from the agent's bundle prompt.md.
func newPromptCmd() *cobra.Command {
	c := &cobra.Command{Use: "prompt", Short: "Manage agents' Langfuse prompt versions"}
	c.AddCommand(&cobra.Command{
		Use:   "push <agent>",
		Short: "Upload <agent>'s bundle prompt.md as a new system/<agent> version, without the production label",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runPromptPush(cmd, args[0]) },
	})
	return c
}

func runPromptPush(cmd *cobra.Command, agent string) error {
	cfg, bundle, err := agentBundle(defaultConfigPath(), agent)
	if err != nil {
		return err
	}
	client, err := promptsClient(cfg)
	if err != nil {
		return err
	}
	art, err := artifactsrc.ResolveBundleFile(cmd.Context(), nil, "system", bundle, "prompt.md")
	if err != nil {
		return err
	}
	name := "system/" + agent
	p, err := client.Push(cmd.Context(), name, art.Body, art.VersionID, []string{langfuse.SeedTag})
	if err != nil {
		return fmt.Errorf("prompt push %s: %w", name, err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s@%d\n", name, p.Version)
	return err
}
