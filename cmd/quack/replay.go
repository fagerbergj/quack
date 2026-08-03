package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/serve"
)

// newReplayCmd: `quack replay <chat-id-or-bundle.zip>` - replay a recorded
// ledger bundle through a fresh in-process server built from the LOCAL
// quack.yaml (same default-path resolution as `server run`/`server
// validate`), with every provider swapped to kind:"replay". Strict by
// default (hermetic, no live calls); --fork-from switches to fork-replay,
// going live from that node's boundary using the SAME local config's real
// provider as the live delegate (internal/inference/factory.go, internal/
// acp's start/startLive split).
func newReplayCmd() *cobra.Command {
	var forkFrom, sourceServer string
	var asJSON bool
	c := &cobra.Command{
		Use:   "replay <chat-id-or-bundle.zip>",
		Short: "Replay a recorded run offline (strict) or live from a changed node (fork)",
		Long: "Replay a recorded session's ledger bundle through a fresh in-process\n" +
			"server: every model/tool/agent call is answered from the recording -\n" +
			"strict, hermetic, no API cost, no live network call - unless --fork-from\n" +
			"names a node, in which case that node's calls (and anything else the\n" +
			"recording no longer matches once its output diverges) go LIVE against\n" +
			"the real provider configured in your quack.yaml instead.\n\n" +
			"Which mode verifies which fix:\n" +
			"  replay-strict (no --fork-from)   verifies a DETERMINISTIC-CODE fix -\n" +
			"                                    the recording should still match\n" +
			"                                    call-for-call; a miss fails loudly.\n" +
			"  fork-replay (--fork-from <node>) verifies a PROMPT/PLAN fix - the\n" +
			"                                    changed node needs a REAL model call\n" +
			"                                    to judge its new behavior (a prompt\n" +
			"                                    edit alone rarely changes the call\n" +
			"                                    sequence, so strict replay would\n" +
			"                                    never naturally diverge there) -\n" +
			"                                    requires a live provider configured\n" +
			"                                    locally.\n\n" +
			"<chat-id-or-bundle.zip> is either a local bundle file (dropped in\n" +
			"testdata/, or downloaded via `quack api GET .../recording`) or a chat\n" +
			"id - fetched from --server (default: the active registered server)'s\n" +
			"recording endpoint.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReplay(cmd, args[0], forkFrom, sourceServer, asJSON)
		},
	}
	c.Flags().StringVar(&forkFrom, "fork-from", "", "fork to LIVE model/agent calls from this node's boundary onward (requires a real provider in quack.yaml)")
	c.Flags().StringVar(&sourceServer, "server", "", "server to fetch the recording from, when the argument is a chat id (default: active registered server)")
	asJSONFlag(c, &asJSON)
	return c
}

func runReplay(cmd *cobra.Command, target, forkFrom, sourceServer string, asJSON bool) error {
	ctx := cmd.Context()
	bundlePath, cleanupBundle, err := resolveBundle(ctx, sourceServer, target)
	if err != nil {
		return err
	}
	defer cleanupBundle()

	sess, err := replay.Load(bundlePath)
	if err != nil {
		return fmt.Errorf("replay: load bundle: %w", err)
	}
	prompt, ok := sess.UserTurn()
	if !ok {
		return fmt.Errorf("replay: bundle %q carries no recorded user turn to replay", bundlePath)
	}

	cfgPath := defaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("no %s found - a replay run needs a LOCAL quack.yaml (run `quack init` first)", cfgPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	replayifyProviders(cfg, bundlePath, forkFrom)

	if forkFrom != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "fork-replay: live from node %q\n", forkFrom)
	} else {
		fmt.Fprintln(cmd.ErrOrStderr(), "replay-strict: hermetic, no live calls")
	}

	base, stop, err := serve.InProcessFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = stop() }()

	code := cli.RunReplay(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), base, prompt, asJSON)
	exitIfNonZero(code)
	return nil
}

// resolveBundle resolves target into a local bundle file path: unchanged if
// it's already a readable file, else fetched as a chat id from
// sourceServer's recording endpoint (cli.Client.FetchRecording) into a temp
// file. cleanup removes that temp file; a no-op for an already-local path.
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
	f, err := os.CreateTemp("", "quack-replay-*.zip")
	if err != nil {
		return "", noop, err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", noop, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// replayifyProviders mutates cfg IN PLACE into a replay config: every named
// provider becomes kind:"replay" pointing at bundlePath. forkFromNode == ""
// is replay-strict (matches .quack/replay-log.md's hermetic guarantee -
// internal/inference/factory.go's kind switch never builds a live delegate
// without fork_mode set); non-empty switches every provider to fork mode,
// carrying the ORIGINAL (real) provider as its `live` delegate - NewModel's
// kind:"replay" case builds that delegate straight from it, so a fork run
// needs no separate provider config of its own beyond what quack.yaml
// already has.
func replayifyProviders(cfg *config.Config, bundlePath, forkFromNode string) {
	for name, orig := range cfg.Providers {
		p := config.ProviderConfig{Kind: "replay", Bundle: bundlePath}
		if forkFromNode != "" {
			live := orig
			p.ForkMode = "fork"
			p.ForkFrom = forkFromNode
			p.Live = &live
		}
		cfg.Providers[name] = p
	}
}
