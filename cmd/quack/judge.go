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
	"github.com/fagerbergj/quack/internal/vetting"
)

// newJudgeCmd: `quack judge replay` - re-grade a recorded chat's judged
// rounds under the working-copy rubric, without re-running the agent.
func newJudgeCmd() *cobra.Command {
	c := &cobra.Command{Use: "judge", Short: "Re-grade recorded rounds against the working-copy rubric"}
	c.AddCommand(newJudgeReplayCmd())
	return c
}

func newJudgeReplayCmd() *cobra.Command {
	var node, rubricPath, sourceServer string
	var round, repeat int
	var deterministicOnly, asJSON bool
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
			return runJudgeReplay(cmd, args[0], node, round, repeat, rubricPath, sourceServer, deterministicOnly, asJSON)
		},
	}
	c.Flags().StringVar(&node, "node", "", "only this node id (default: every judged node)")
	c.Flags().IntVar(&round, "round", 0, "only this judge round number, e.g. 2 for judge-r2 (default: every round)")
	c.Flags().StringVar(&rubricPath, "rubric", "", "rubric.yaml to replay against (default: the graded node's own bundled rubric)")
	c.Flags().StringVar(&sourceServer, "from-server", "", "server to fetch the recording from, when the argument is a chat id (default: active registered server)")
	c.Flags().BoolVar(&deterministicOnly, "deterministic-only", false, "replay only the code-owned criteria; never touches the judge model")
	c.Flags().IntVar(&repeat, "repeat", 1, "judge each round N times and print every judge-scored criterion's spread (mean, sd, pass rate)")
	asJSONFlag(c, &asJSON)
	return c
}

func runJudgeReplay(cmd *cobra.Command, target string, node string, round, repeat int, rubricPath, sourceServer string, deterministicOnly, asJSON bool) error {
	ctx := cmd.Context()
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

	var judge vetting.JudgeFactory
	var judgeArtifactTools []tool.Tool
	var hasRealArtifactAccess bool
	if !deterministicOnly {
		judge, err = cli.BuildReplayJudge(cfg, func(p config.ProviderConfig, m string) (model.LLM, error) {
			return inference.NewModelWithEffort(p, m, nil, cfg.ModelCost(m), cfg.ModelEffort(m))
		})
		if err != nil {
			return err
		}
		if judgeArtifactTools, hasRealArtifactAccess, err = judgeArtifactToolsFor(ctx, sourceServer, target); err != nil {
			return err
		}
	}

	opts := cli.ReplayOptions{Node: node, Round: round, RubricPath: rubricPath, DeterministicOnly: deterministicOnly, Repeat: repeat}
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
