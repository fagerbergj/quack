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
	"github.com/fagerbergj/quack/internal/vetting"
)

// newLedgerCmd: `quack ledger list|export|show|rebuild|recover`. list and
// export talk to a running server; show/rebuild/recover have no REST surface
// and run server-side against the SAME stores a LOCAL quack.yaml would boot `quack serve` against (config.Load(defaultConfigPath()), like `quack replay`) - there is no notion of "the active registered server" here, since the point is direct store access, not an HTTP round-trip.
func newLedgerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ledger",
		Short: "Inspect or regenerate the write-ahead ledger's projections",
	}
	c.AddCommand(newLedgerListCmd(), newLedgerExportCmd(), newLedgerShowCmd(), newLedgerRebuildCmd(), newLedgerRecoverCmd())
	return c
}

// newLedgerRecoverCmd: `quack ledger recover [chat-id] [--dry-run]` - the
// same cli.Recover the server runs at boot: delivery.intent entries with no
// completing delivery_record revision are checked against the configured extension's DeliveryRecoverer, artifact.revision intents with no store row are marked aborted. Redo stays nil: redoing a delivery needs the live node context this offline command doesn't have. --dry-run reports without calling the extension or writing. A recoverer-build failure degrades to a stderr warning rather than aborting, so a misconfigured extension cannot hide the orphans it might otherwise explain.
func newLedgerRecoverCmd() *cobra.Command {
	var dryRun, asJSON bool
	c := &cobra.Command{
		Use:   "recover [chat-id]",
		Short: "Settle intents whose projection write is missing (a crashed delivery or artifact save)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, st, artifacts, err := openLedgerAndStores()
			if err != nil {
				return err
			}
			proj := cli.Projections{ArtifactRowExists: cli.ArtifactRowChecker(st, artifacts), Delivery: buildRecovererOrWarn(cmd, dryRun), ChatExists: st.ChatExists}
			proj.DeliveryRecorded, proj.RecordDelivery = vetting.DeliveryProjections(artifacts, ls, st.SessionUserForChat)
			sum, err := cli.Recover(cmd.Context(), ls, args, proj, dryRun)
			if err != nil {
				return err
			}
			if asJSON {
				return cli.WriteJSON(cmd.OutOrStdout(), sum)
			}
			if _, err := fmt.Fprint(cmd.OutOrStdout(), cli.FormatRecoverSummary(sum)); err != nil {
				return err
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report orphaned intents only; never call the extension or write to the ledger")
	asJSONFlag(c, &asJSON)
	return c
}

// buildRecovererOrWarn builds the configured extension's DeliveryRecoverer,
// or returns nil (every delivery orphan reported Unresolved) rather than an
// error - see newLedgerRecoverCmd's doc.
func buildRecovererOrWarn(cmd *cobra.Command, dryRun bool) cli.DeliveryRecoverer {
	if dryRun {
		return nil
	}
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ledger recover: load config: %v; continuing without a recoverer, delivery orphans will be reported Unresolved\n", err)
		return nil
	}
	built, name, err := serve.BuildDeliveryRecoverer(cfg)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ledger recover: build delivery recoverer: %v; continuing without one, delivery orphans will be reported Unresolved\n", err)
		return nil
	}
	if built == nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "no configured extension implements DeliveryRecoverer; every delivery orphan will be Unresolved")
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "using extensions.%s as the delivery recoverer\n", name)
	return built
}

func newLedgerListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List chats with a recording on the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunLedgerList(cmd.Context(), cmd.OutOrStdout(), t, asJSON)
			})
		},
	}
	asJSONFlag(c, &asJSON)
	return c
}

// newLedgerExportCmd: `ledger export <chat-id> [-o file]` - the dogfooding
// ritual for the replay engine: hit a bug -> export the chat -> attach the
// zip to the issue or pin it in testdata/ as a replay fixture.
func newLedgerExportCmd() *cobra.Command {
	var output string
	c := &cobra.Command{
		Use:   "export <chat-id>",
		Short: "Download a chat's recording bundle",
		Long: "Download a chat's recording bundle (a ZIP: manifest.json + entries.jsonl of\n" +
			"typed ledger entries) - default filename <chat-id>.zip. Only bundles at the\n" +
			"current ledger_version replay; older OTel-attribute bundles are unsupported.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunLedgerExport(cmd.Context(), cmd.OutOrStdout(), t, args[0], output)
			})
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "output file path (default: <chat-id>.zip)")
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
	var dryRun, asJSON bool
	c := &cobra.Command{
		Use:   "rebuild <chat-id>",
		Short: "Reset a chat's projection watermarks and re-fold from the ledger",
		Long: "Resets the chat's artifact and SSE projection watermarks to 0 and folds\n" +
			"from the start (#1144 P3): every artifact revision's kind/class/lineage is\n" +
			"rewritten from the fold, and a MISSING node lifecycle row\n" +
			"(node_start/node_done/node_failed) is inserted. Bytes and revision numbers\n" +
			"are never touched, and no OTHER row - lifecycle or observational\n" +
			"(agent_token, tool calls, dag_plan, ...) - is ever deleted or overwritten.\n" +
			"--dry-run reports what would change without writing or resetting anything.\n\n" +
			"Run this with the server STOPPED, or with no active run on this chat:\n" +
			"writes are not transactional against a concurrently running run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, st, artifacts, err := openLedgerAndStores()
			if err != nil {
				return err
			}
			report, err := cli.RunLedgerRebuild(cmd.Context(), ls, st, artifacts, args[0], dryRun)
			if err != nil {
				return err
			}
			if asJSON {
				if err := cli.WriteJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), cli.FormatLedgerRebuildReport(report)); err != nil {
					return err
				}
			}
			if len(report.ArtifactUpdateErrors) > 0 {
				return fmt.Errorf("ledger rebuild: %d artifact revision(s) failed to update", len(report.ArtifactUpdateErrors))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	asJSONFlag(c, &asJSON)
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
