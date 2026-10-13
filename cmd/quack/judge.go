package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/serve"
	"github.com/fagerbergj/quack/internal/vetting"
)

// newJudgeCmd: `quack judge replay` - re-grade a recorded chat's judged
// rounds under the working-copy rubric, without re-running the agent.
func newJudgeCmd() *cobra.Command {
	c := &cobra.Command{Use: "judge", Short: "Re-grade recorded rounds against the working-copy rubric"}
	c.AddCommand(newJudgeReplayCmd(), newJudgeScoreGoldenCmd())
	return c
}

func newJudgeScoreGoldenCmd() *cobra.Command {
	var deterministicOnly, asJSON bool
	var outPath string
	c := &cobra.Command{
		Use:   "score-golden <dir>",
		Short: "Grade a labeled golden set and report false-pass/false-fail rates per criterion",
		Long: "Grades every item in <dir> (config.json plus one JSON file per labeled item) through the\n" +
			"live gate's own functions and compares each criterion's pass/fail at the set's threshold with\n" +
			"the human label. Exits 2 when a false-pass or false-fail rate, per criterion or overall,\n" +
			"exceeds the set's limit, 1 when an item errored or nothing was scored. --deterministic-only\n" +
			"skips items that need the judge model and needs no quack.yaml; otherwise the local quack.yaml's\n" +
			"gates.judge model grades the items marked needs_live_model.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScoreGolden(cmd, args[0], deterministicOnly, asJSON, outPath)
		},
	}
	c.Flags().BoolVar(&deterministicOnly, "deterministic-only", false, "score only the code-owned criteria; skip items that need the judge model")
	c.Flags().StringVar(&outPath, "out", "", "also write the JSON report to this file")
	asJSONFlag(c, &asJSON)
	return c
}

func runScoreGolden(cmd *cobra.Command, dir string, deterministicOnly, asJSON bool, outPath string) error {
	gcfg, items, err := cli.LoadGoldenSet(dir)
	if err != nil {
		return err
	}
	var base vetting.Config
	var judge vetting.JudgeFactory
	var artifactTools []tool.Tool
	if !deterministicOnly {
		if base, judge, artifactTools, err = goldenJudge(cmd.Context()); err != nil {
			return err
		}
	}
	rep, code := cli.ScoreGolden(cmd.Context(), gcfg, items, base, judge, artifactTools)
	if asJSON {
		err = cli.WriteJSON(cmd.OutOrStdout(), rep)
	} else {
		cli.RenderGoldenReport(cmd.OutOrStdout(), rep)
	}
	if err == nil && outPath != "" {
		err = cli.WriteJSONFile(outPath, rep)
	}
	if err != nil {
		return err
	}
	exitIfNonZero(code)
	return nil
}

// goldenJudge builds the local quack.yaml's gate config and judge model, as judge replay does.
func goldenJudge(ctx context.Context) (vetting.Config, vetting.JudgeFactory, []tool.Tool, error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
	if err != nil {
		return vetting.Config{}, nil, nil, fmt.Errorf("score-golden needs a local quack.yaml with gates.judge, or --deterministic-only: %w", err)
	}
	promptbuilder.SetLocation(cfg.Location())
	base, err := vetting.FromConfig(ctx, nil, cfg.Gates)
	if err != nil {
		return base, nil, nil, err
	}
	judge, err := cli.BuildReplayJudge(cfg, func(p config.ProviderConfig, m string) (model.LLM, error) {
		return inference.NewModel(p, m, nil, cfg.ModelCost(m), cfg.ModelEffort(m))
	})
	if err == nil && judge == nil {
		err = fmt.Errorf("score-golden: gates.judge is not configured in %s; pass --deterministic-only", cfgPath)
	}
	if err != nil {
		return base, nil, nil, err
	}
	stub, err := cli.StubArtifactTools()
	return base, judge, stub, err
}

func newJudgeReplayCmd() *cobra.Command {
	var node, rubricPath, sourceServer, outPath string
	var round, repeat int
	var deterministicOnly, asJSON, verify bool
	c := &cobra.Command{
		Use:   "replay <chat-id-or-bundle.zip>",
		Short: "Re-grade a recorded chat's judged rounds under the working-copy rubric",
		Long: "Rebuilds each judged round's gate inputs (the answer, the worker's\n" +
			"retrieval activity, the question) from a chat's recording, then runs the\n" +
			"SAME gate functions the live trust gate runs - computeDeterministicCriteria\n" +
			"and, unless --deterministic-only, the judge model - against the working-copy\n" +
			"rubric (the agent's bundled rubric.yaml by default, or --rubric). Prints each\n" +
			"criterion's recorded vs. replayed score and pass/fail, and exits non-zero the\n" +
			"moment any flips - proof that a rubric or gate change reproduces (or changes) past verdicts.\n\n" +
			"Read-only against the source chat: replay never writes to it. --deterministic-only\n" +
			"skips the judge model entirely, so it runs with no model endpoint configured.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := cli.ReplayOptions{Node: node, Round: round, RubricPath: rubricPath, DeterministicOnly: deterministicOnly, Repeat: repeat, ReportPath: outPath}
			return runJudgeReplay(cmd, args[0], opts, sourceServer, verify, asJSON)
		},
	}
	c.Flags().StringVar(&node, "node", "", "only this node id (default: every judged node)")
	c.Flags().IntVar(&round, "round", 0, "only this judge round number, e.g. 2 for judge-r2 (default: every round)")
	c.Flags().StringVar(&rubricPath, "rubric", "", "rubric.yaml to replay against (default: the graded node's own bundled rubric)")
	c.Flags().StringVar(&sourceServer, "from-server", "", "server to fetch the recording from, when the argument is a chat id (default: --server, else the active registered server)")
	c.Flags().BoolVar(&deterministicOnly, "deterministic-only", false, "replay only the code-owned criteria; never touches the judge model")
	c.Flags().IntVar(&repeat, "repeat", 1, "judge each round N times and report every judge-scored criterion's spread (mean, sd, pass rate, agreement) plus an overall consistency summary")
	c.Flags().StringVar(&outPath, "out", "", "also write the JSON report to this file, to store and diff against a later run")
	c.Flags().BoolVar(&verify, "verify", false, "run the shadow verify tier: the judge model reads each located specific's evidence window and answers with a quote code checks (model calls; works with --deterministic-only)")
	asJSONFlag(c, &asJSON)
	return c
}

func runJudgeReplay(cmd *cobra.Command, target string, opts cli.ReplayOptions, sourceServer string, verify, asJSON bool) error {
	ctx := cmd.Context()
	sourceServer = recordingSource(cmd, sourceServer)
	cfgPath := defaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("no %s found - judge replay needs a LOCAL quack.yaml (run `quack init` first)", cfgPath)
	}
	bundlePath, cleanup, err := resolveBundle(ctx, sourceServer, target)
	if err != nil {
		return err
	}
	defer cleanup()

	sess, err := bundle.Load(bundlePath)
	if err != nil {
		return fmt.Errorf("judge replay: load bundle: %w", err)
	}
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
	if err != nil {
		return err
	}
	// Seed plugin agents from their local clones as `server validate` does, so their bundled rubric resolves.
	plugins, _, err := serve.ResolveConfiguredPlugins(cfg)
	if err == nil {
		_, _, err = serve.SeedPluginAgentsAndShapes(cfg, plugins)
	}
	if err != nil {
		return err
	}
	promptbuilder.SetLocation(cfg.Location())

	var judge vetting.JudgeFactory
	var judgeArtifactTools []tool.Tool
	var hasRealArtifactAccess bool
	if !opts.DeterministicOnly {
		judge, err = cli.BuildReplayJudge(cfg, func(p config.ProviderConfig, m string) (model.LLM, error) {
			return inference.NewModel(p, m, nil, cfg.ModelCost(m), cfg.ModelEffort(m))
		})
		if err != nil {
			return err
		}
		if judgeArtifactTools, hasRealArtifactAccess, err = judgeArtifactToolsFor(ctx, sourceServer, target); err != nil {
			return err
		}
	}

	opts.Pages = replayPagesFor(ctx, target, sourceServer)
	if verify {
		if opts.Pages == nil {
			return fmt.Errorf("--verify needs stored pages: replay a chat id with --from-server, not a local bundle")
		}
		if opts.Verifier, err = cli.BuildReplayVerifier(cfg, func(p config.ProviderConfig, m string) (model.LLM, error) {
			return inference.NewModel(p, m, nil, cfg.ModelCost(m), cfg.ModelEffort(m))
		}); err != nil {
			return err
		}
		if opts.Verifier == nil {
			return fmt.Errorf("--verify needs gates.judge configured in %s", cfgPath)
		}
	}
	code := cli.RunJudgeReplay(ctx, cfg, sess, opts, judge, judgeArtifactTools, hasRealArtifactAccess, cmd.OutOrStdout(), asJSON)
	exitIfNonZero(code)
	return nil
}

// judgeArtifactToolsFor gives the judge real REST read access for a chat id,
// or (a local bundle file) a stub answering "no artifacts available".
func judgeArtifactToolsFor(ctx context.Context, sourceServer, target string) (tools []tool.Tool, real bool, err error) {
	if st, statErr := os.Stat(target); statErr == nil && !st.IsDir() {
		tools, err = cli.StubArtifactTools()
		return tools, false, err
	}
	c, err := cli.NewClient(ctx, sourceServer)
	if err != nil {
		return nil, false, err
	}
	tools, err = cli.RESTArtifactTools(c, target)
	return tools, true, err
}

// replayPagesFor: stored web pages for the shadow locate tier come from the
// server's artifact API when the target is a chat id; a local bundle has none.
func replayPagesFor(ctx context.Context, target, sourceServer string) vetting.PageLoader {
	if st, err := os.Stat(target); err == nil && !st.IsDir() {
		return nil
	}
	c, err := cli.NewClient(ctx, sourceServer)
	if err != nil {
		return nil
	}
	return cli.RESTPages{Client: c, ChatID: target}
}
