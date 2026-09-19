package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/eval"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/serve"
)

// newEvalCmd re-runs a recorded bundle's user turns LIVE through a fresh
// in-process server built from the LOCAL quack.yaml, with --role's model
// swapped in, then compares the fresh run's judge scores against the recording's own. Every model/tool/agent call is live - only the recorded USER TURNS are consumed from the bundle.
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
			"<chat-id-or-bundle.zip> is either a local bundle file (dropped in\n" +
			"testdata/, or downloaded via `quack api GET .../recording`) or a chat\n" +
			"id - fetched from --from-server (default: the active registered\n" +
			"server)'s recording endpoint.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval(cmd, args[0], model, role, sourceServer, asJSON)
		},
	}
	c.Flags().StringVar(&model, "model", "", "model name to swap in for --role (required)")
	c.Flags().StringVar(&role, "role", eval.RoleAll, "which agents to swap: coder, researcher, orch, or all")
	c.Flags().StringVar(&sourceServer, "from-server", "", "server to fetch the recording from, when the argument is a chat id (default: active registered server) - distinct from the global --server")
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

	sess, err := bundle.Load(bundlePath)
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
	cfg, err := config.LoadDeferringAgentCompleteness(cfgPath)
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

// resolveBundle resolves target into a local bundle file path: unchanged if
// it's already a readable file, else fetched as a chat id from
// sourceServer's recording endpoint (cli.Client.FetchRecording) into a temp file. cleanup removes that temp file; a no-op for an already-local path.
func resolveBundle(ctx context.Context, sourceServer, target string) (path string, cleanup func(), err error) {
	noop := func() {}
	if st, statErr := os.Stat(target); statErr == nil && !st.IsDir() {
		return target, noop, nil
	}
	c, err := cli.NewClient(ctx, sourceServer)
	if err != nil {
		return "", noop, err
	}
	body, err := c.FetchRecording(ctx, target)
	if err != nil {
		return "", noop, fmt.Errorf("fetch recording for chat %q: %w", target, err)
	}
	f, err := os.CreateTemp("", "quack-eval-*.zip")
	if err != nil {
		return "", noop, err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", noop, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}
