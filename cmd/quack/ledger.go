package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/serve"
	"github.com/fagerbergj/quack/internal/store"
)

// newLedgerCmd: `quack ledger show|rebuild` - #1101's read/write access to
// the write-ahead ledger. No REST surface exists for either yet, so both run
// server-side against the SAME stores a LOCAL quack.yaml would boot `quack
// serve` against (config.Load(defaultConfigPath()), like `quack replay`) -
// there is no notion of "the active registered server" here, since the
// point is direct store access, not an HTTP round-trip.
func newLedgerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ledger",
		Short: "Inspect or regenerate the write-ahead ledger's projections",
	}
	c.AddCommand(newLedgerShowCmd(), newLedgerRebuildCmd(), newLedgerRecoverCmd())
	return c
}

// newLedgerRecoverCmd: `quack ledger recover <chat>` (#1093 case 13) - finds
// delivery.intent entries with no matching delivery.done (a run that died
// between the two) and reports them. recoverer/redoFunc are nil here: no
// extension in this build implements cli.DeliveryRecoverer yet (that lands
// with the sdk/github bump #1093's PR describes), and redoing a delivery
// needs the live node context this offline command doesn't have - every
// orphan is reported Unresolved until that wiring exists. Known ceiling,
// not a bug: the reconciliation ALGORITHM is what this command proves out.
func newLedgerRecoverCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recover <chat-id>",
		Short: "Reconcile delivery.intent entries with no delivery.done (a crashed delivery)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, _, _, err := openLedgerAndStores()
			if err != nil {
				return err
			}
			report, err := cli.RunLedgerRecover(cmd.Context(), ls, args[0], nil, nil)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), cli.FormatLedgerRecoverReport(report))
			return nil
		},
	}
	return c
}

func newLedgerShowCmd() *cobra.Command {
	var fromSeq int64
	c := &cobra.Command{
		Use:   "show <chat-id>",
		Short: "Print a chat's raw ledger entries (one JSON object per line)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, _, _, err := openLedgerAndStores()
			if err != nil {
				return err
			}
			return cli.RunLedgerShow(cmd.Context(), cmd.OutOrStdout(), ls, args[0], fromSeq)
		},
	}
	c.Flags().Int64Var(&fromSeq, "from-seq", 0, "only entries with seq >= this value")
	return c
}

func newLedgerRebuildCmd() *cobra.Command {
	var dryRun, force bool
	c := &cobra.Command{
		Use:   "rebuild <chat-id>",
		Short: "Reconcile a chat's artifact metadata and SSE table against the ledger",
		Long: "Diffs the ledger fold (V4 §4.9's projections) against what's actually\n" +
			"stored and fixes only real drift: an artifact revision's kind/class/\n" +
			"lineage is corrected if (and only if) it differs, and a MISSING node\n" +
			"lifecycle row (node_start/node_done/node_failed) is inserted. Bytes and\n" +
			"revision numbers are never touched, and no OTHER row - lifecycle or\n" +
			"observational (agent_token, tool calls, dag_plan, ...) - is ever deleted\n" +
			"or overwritten. --dry-run reports what would change without writing;\n" +
			"zero changes is the expected result on a healthy chat.\n\n" +
			"--force switches to the OLD, DESTRUCTIVE mode: it WIPES the chat's\n" +
			"entire chat_events table and replaces it with synthesized lifecycle\n" +
			"rows only - every observational event is LOST. Use only when you have\n" +
			"already decided the chat's table is unrecoverable any other way.\n\n" +
			"Run this with the server STOPPED, or with no active run on this chat:\n" +
			"writes are not transactional against a concurrently running run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, st, artifacts, err := openLedgerAndStores()
			if err != nil {
				return err
			}
			if force {
				fmt.Fprintln(cmd.ErrOrStderr(), "--force: replacing the whole chat_events table - all observational history (tokens, tool calls, dag_plan, ...) for this chat will be lost")
			}
			report, err := cli.RunLedgerRebuild(cmd.Context(), ls, st, artifacts, args[0], dryRun, force)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), cli.FormatLedgerRebuildReport(report))
			if len(report.ArtifactUpdateErrors) > 0 {
				return fmt.Errorf("ledger rebuild: %d artifact revision(s) failed to update", len(report.ArtifactUpdateErrors))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	c.Flags().BoolVar(&force, "force", false, "DESTRUCTIVE: wipe and replace the whole chat_events table (loses observational history)")
	return c
}

// openLedgerAndStores builds the ledger store, session store and artifact
// service from the local quack.yaml - the same wiring internal/serve.Run
// uses, minus the HTTP server and orchestrator (neither show nor rebuild
// runs anything).
func openLedgerAndStores() (ls ledger.LedgerStore, st *store.Store, artifacts *store.TurnAwareService, err error) {
	cfgPath := defaultConfigPath()
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		return nil, nil, nil, fmt.Errorf("no %s found - `quack ledger` needs a LOCAL quack.yaml pointing at the same stores your server uses", cfgPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}

	ls = serve.LedgerStoreFromConfig(cfg)
	if ls == nil {
		return nil, nil, nil, fmt.Errorf("ledger: no recording store configured (observability.recording.store in %s)", cfgPath)
	}

	sessionStore, ok := cfg.Store(cfg.Session.Store)
	if !ok {
		return nil, nil, nil, fmt.Errorf("session store %q not found in stores registry", cfg.Session.Store)
	}
	st, err = store.New(sessionStore.Kind, sessionStore.URL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store open failed: %w", err)
	}

	artifactSvc, err := serve.BuildArtifactService(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("artifact service init failed: %w", err)
	}
	artifacts = store.NewTurnAwareService(artifactSvc)
	return ls, st, artifacts, nil
}
