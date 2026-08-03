package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/eval"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/serve"
)

// newEvalCmd: `quack eval <chat-id-or-bundle.zip> --model <name> [--role
// coder|researcher|orch|all]` (#606) - re-run a recorded bundle's user
// turns LIVE through a fresh in-process server built from the LOCAL
// quack.yaml, with --role's model swapped in, then compare the fresh run's
// judge scores against the recording's own. Unlike `quack replay`, nothing
// here is a replay provider: every model/tool/agent call is live - eval
// exists to test whether a DIFFERENT model does better or worse than what
// was recorded, so a hermetic/stubbed run would defeat the point. Only the
// recorded USER TURNS are consumed from the bundle (.quack/replay-log.md:
// "eval mode consumes ONLY recorded user turns; everything runs live").
func newEvalCmd() *cobra.Command {
	var model, role, sourceServer string
	var asJSON bool
	c := &cobra.Command{
		Use:   "eval <chat-id-or-bundle.zip>",
		Short: "Re-run a recorded conversation live with a swapped model and compare judge scores",
		Long: "Feeds a bundle's recorded user turns, in order, into a FRESH chat on a\n" +
			"server built from your LOCAL quack.yaml with --role's model(s) swapped\n" +
			"to --model. Tools and every other agent stay live and unchanged - only\n" +
			"the targeted role's model differs from the recording. The judge itself\n" +
			"is NEVER swapped, so the two runs' scores are comparable.\n\n" +
			"When the run finishes, the fresh chat's own recording is fetched and\n" +
			"scored the same way the original bundle was, and a per-criterion\n" +
			"recorded-vs-new table is printed. Exit code is 0 whenever the eval\n" +
			"itself completed - a WORSE score is a result, not a failure.\n\n" +
			"<chat-id-or-bundle.zip> resolves exactly like `quack replay`'s argument:\n" +
			"a local bundle file, or a chat id fetched from --server's recording\n" +
			"endpoint.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval(cmd, args[0], model, role, sourceServer, asJSON)
		},
	}
	c.Flags().StringVar(&model, "model", "", "model name to swap in for --role (required)")
	c.Flags().StringVar(&role, "role", eval.RoleAll, "which agents to swap: coder, researcher, orch, or all")
	c.Flags().StringVar(&sourceServer, "server", "", "server to fetch the recording from, when the argument is a chat id (default: active registered server)")
	asJSONFlag(c, &asJSON)
	_ = c.MarkFlagRequired("model")
	return c
}

func runEval(cmd *cobra.Command, target, model, role, sourceServer string, asJSON bool) error {
	ctx := cmd.Context()
	bundlePath, cleanupBundle, err := resolveBundle(ctx, sourceServer, target)
	if err != nil {
		return err
	}
	defer cleanupBundle()

	sess, err := replay.Load(bundlePath)
	if err != nil {
		return fmt.Errorf("eval: load bundle: %w", err)
	}
	turns := sess.UserTurns()
	if len(turns) == 0 {
		return fmt.Errorf("eval: bundle %q carries no recorded user turn to re-run", bundlePath)
	}
	recordedScores := sess.EvaluationResults()
	recordedAnswer, _ := sess.FinalAnswer()

	cfgPath := defaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("no %s found - an eval run needs a LOCAL quack.yaml (run `quack init` first)", cfgPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	changed, err := eval.OverrideModel(cfg, role, model)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return fmt.Errorf("eval: --role %q matched no agents in %s", role, cfgPath)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "eval: role=%s model=%s agents=%v, %d recorded turn(s)\n", role, model, changed, len(turns))

	base, stop, err := serve.InProcessFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = stop() }()

	code := cli.RunEval(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), base, role, model, changed, turns, recordedScores, recordedAnswer, asJSON)
	exitIfNonZero(code)
	return nil
}
