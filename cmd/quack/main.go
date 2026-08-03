// Command quack is Quack's one-binary CLI and server. `quack server run` runs the
// REST + MCP API and the embedded SPA; the other verbs (`chat`, `api`, `server`)
// drive a running server over HTTP + SSE. There is no TUI: `-p`, `chat send`,
// and `chat show` are the interface, and their pause/failure exit codes
// (0/1/2 - see internal/cli's Report) make them pipeable and scriptable. This
// file is the cobra wiring only - command funcs stay thin and dispatch into
// internal/cli (see the quack-cli skill).
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/serve"
	"github.com/fagerbergj/quack/internal/wizard"
	"github.com/fagerbergj/quack/internal/workspace"
)

// version is the build stamp, overridden at release time via
//
//	-ldflags "-X main.version=v1.2.3"
//
// so `quack version` and the server report the same string (M10).
var version = "dev"

func main() {
	// Threaded through to internal/serve so a recording bundle's
	// manifest.json (GetChatRecording) reports the same build stamp as
	// `quack version`.
	serve.Version = version
	// __sandbox-exec mode, dispatched on argv[1] BEFORE cobra: the Landlock
	// self-exec (see workspace.RunSandboxExecIfInvoked) - never returns on
	// success (syscall.Exec replaces this process).
	workspace.RunSandboxExecIfInvoked()
	// GIT_ASKPASS mode, dispatched on argv[0] BEFORE cobra: when this binary
	// is exec'd through the workspace askpass symlink (git runs $GIT_ASKPASS
	// directly as one program path - see cmd/quack/git_askpass.go), answer
	// git's credential prompt and exit.
	if isGitAskpassInvocation() {
		gitAskpassMain(os.Args, os.Stdout)
		return
	}
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1) // cobra has already printed the error to stderr
	}
}

func newRootCmd() *cobra.Command {
	var printPrompt string
	root := &cobra.Command{
		Use:   "quack",
		Short: "Quack - specialist AI agents, one binary",
		Long: "Quack is a one-binary CLI and server for agent-driven work - web research,\n" +
			"coding, and more. There is no TUI:\n" +
			"-p, `chat send`, and `chat show` ARE the interface - pipeable, scriptable exit\n" +
			"codes (0 answered, 1 failed, 2 paused on a question).\n\n" +
			"  quack init                       get configured (run locally or connect to a server)\n" +
			"  quack server run                 run the API + SPA server (foreground)\n" +
			"  quack -p \"<prompt>\"               one-shot prompt, print and exit\n" +
			"  quack chat new                   create a chat, print its id\n" +
			"  quack chat send <id> \"<msg>\"      send a message (or answer a paused question)\n" +
			"  quack chat show <id> [-f]         status snapshot, optionally follow the live run\n" +
			"  quack chat list                  list chats with their status\n" +
			"  quack chat|server|api            manage chats, the server, and raw API calls",
		SilenceUsage: true, // a failing RunE is an error, not a usage mistake
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printPrompt == "" {
				return cmd.Help()
			}
			server, _ := cmd.Flags().GetString("server")
			target, stop, err := resolveTarget(cmd.Context(), server)
			if err != nil {
				return err
			}
			defer stop()
			var events io.Writer
			if showEvents, _ := cmd.Flags().GetBool("events"); showEvents {
				events = cmd.ErrOrStderr() // pipeline trace → stderr; answer → stdout
			}
			attach, _ := cmd.Flags().GetStringSlice("attach")
			asJSON, _ := cmd.Flags().GetBool("json")
			code := cli.PrintPrompt(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), events, target, printPrompt, attach, asJSON)
			exitIfNonZero(code)
			return nil
		},
	}
	// Client context: which server to talk to. Resolution (flag → config → default)
	// lands with internal/cli; here it is just declared so subcommands inherit it.
	root.PersistentFlags().String("server", "", "quack server URL (default: active server from config, else http://localhost:8080)")
	root.Flags().StringVarP(&printPrompt, "print", "p", "", "one-shot prompt; print the result and exit (no TUI)")
	root.Flags().Bool("events", false, "with -p: also print the pipeline event trace (plan, node lifecycle) to stderr")
	root.Flags().StringSlice("attach", nil, "with -p: attach file(s) - image/audio - to the prompt (repeatable)")
	root.Flags().Bool("json", false, "with -p: print one JSON result object instead of plain text (same exit codes)")

	root.AddCommand(newInitCmd(), newChatCmd(), newRecordingCmd(), newServerCmd(), newAPICmd(), newVersionCmd(), newGitAskpassCmd())
	return root
}

// exitIfNonZero calls os.Exit(code) for a non-zero code - used by the
// bulletproof-CLI commands (-p, chat send, chat show) whose exit codes (0
// answered / 1 failed / 2 paused) don't fit cobra's error-only exit(1) model.
// A zero code returns normally so deferred cleanup (e.g. an in-process
// server's teardown) still runs.
func exitIfNonZero(code int) {
	if code != 0 {
		os.Exit(code)
	}
}

// newInitCmd: `quack init` - onboarding. Local (run quack here → server wizard
// + register localhost) or Remote (register a server someone else runs).
func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Get quack configured - run locally or connect to a hosted server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, _ := cmd.Flags().GetString("output")
			force, _ := cmd.Flags().GetBool("force")
			return wizard.ClientInit(cmd.Context(), out, force)
		},
	}
	c.Flags().StringP("output", "o", "quack.yaml", "path to write quack.yaml (local)")
	c.Flags().Bool("force", false, "overwrite an existing quack.yaml")
	return c
}

// newChatCmd: create and drive chat sessions, plus per-node lifecycle control.
func newChatCmd() *cobra.Command {
	c := &cobra.Command{Use: "chat", Short: "Create and drive chat sessions"}

	node := &cobra.Command{Use: "node", Short: "Control nodes within a chat's active run"}
	node.AddCommand(
		newNodeStopCmd(), newNodePauseCmd(), newNodeResumeCmd(),
		newNodeQueueCmd(), newNodeQueueEditCmd(), newNodeQueueRemoveCmd(),
		newNodeEditCmd(), newNodeRetryCmd(),
	)

	c.AddCommand(
		newChatNewCmd(),
		newChatSendCmd(),
		newChatShowCmd(),
		newChatListCmd(),
		newChatExportCmd(),
		newChatStopCmd(),
		newChatDeleteCmd(),
		node,
	)
	return c
}

func newChatNewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new",
		Short: "Create a chat and print its id (create-only - send the first message with `chat send`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunChatNew(cmd.Context(), cmd.OutOrStdout(), t)
			})
		},
	}
}

// newChatSendCmd: `chat send <id> "<msg>"` - the non-interactive way to answer
// a needs_input question or ask a follow-up (see internal/cli's Report for the
// 0/1/2 exit-code semantics).
func newChatSendCmd() *cobra.Command {
	var asJSON, showEvents bool
	var attach []string
	c := &cobra.Command{
		Use:   "send <id> <message>",
		Short: "Send a non-interactive message to an existing chat (also how you answer a paused question)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				code := cli.RunChatSend(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), t, args[0], args[1], attach, showEvents, asJSON)
				exitIfNonZero(code)
				return nil
			})
		},
	}
	asJSONFlag(c, &asJSON)
	c.Flags().BoolVar(&showEvents, "events", false, "also print the pipeline event trace (plan, node lifecycle) to stderr")
	c.Flags().StringSliceVar(&attach, "attach", nil, "attach file(s) - image/audio - to the message (repeatable)")
	return c
}

// newChatShowCmd: `chat show <id>` - a status snapshot (id/title/status/pending
// question, the last turn's per-node table, then its answer), or the full
// ChatDetail with --json. -f/--follow attaches to the live stream after the
// snapshot (replaces the TUI's live view).
func newChatShowCmd() *cobra.Command {
	var asJSON, follow bool
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a chat's status snapshot (id/title/status/pending question, node table, last answer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				code := cli.RunChatShow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), t, args[0], asJSON, follow)
				exitIfNonZero(code)
				return nil
			})
		},
	}
	asJSONFlag(c, &asJSON)
	c.Flags().BoolVarP(&follow, "follow", "f", false, "after the snapshot, attach to the chat's live stream until the run ends")
	return c
}

// withTarget runs fn against a resolved server (remote or in-process duck),
// tearing the in-process server down afterward.
func withTarget(cmd *cobra.Command, fn func(target string) error) error {
	server, _ := cmd.Flags().GetString("server")
	target, stop, err := resolveTarget(cmd.Context(), server)
	if err != nil {
		return err
	}
	defer stop()
	return fn(target)
}

func newChatListCmd() *cobra.Command {
	var asJSON bool
	var filter, status, repo, kind string
	c := &cobra.Command{
		Use:   "list",
		Short: "List chats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunChatList(cmd.Context(), cmd.OutOrStdout(), t, asJSON, cli.NewChatListFilters(filter, status, repo, kind))
			})
		},
	}
	asJSONFlag(c, &asJSON)
	c.Flags().StringVar(&filter, "filter", "all", "filter by origin: all, github, direct")
	c.Flags().StringVar(&status, "status", "", "filter by status: idle, running, needs_input, failed")
	c.Flags().StringVar(&repo, "repo", "", "filter to a github repo (owner/repo); github chats only")
	c.Flags().StringVar(&kind, "type", "", "filter by github ref type: issue, pr")
	return c
}

func newChatExportCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "export <id>",
		Short: "Export a chat transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunChatExport(cmd.Context(), cmd.OutOrStdout(), t, args[0], asJSON)
			})
		},
	}
	asJSONFlag(c, &asJSON)
	return c
}

func newChatStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a chat's active run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunChatStop(cmd.Context(), cmd.OutOrStdout(), t, args[0])
			})
		},
	}
}

func newChatDeleteCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a chat (irreversible)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunChatDelete(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), t, args[0], yes)
			})
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// asJSONFlag registers a standard --json flag bound to dst.
func asJSONFlag(c *cobra.Command, dst *bool) {
	c.Flags().BoolVar(dst, "json", false, "output raw JSON")
}

// newRecordingCmd: the replay ledger's CLI surface - list recorded sessions
// and export one as a bundle for offline replay/fixtures.
func newRecordingCmd() *cobra.Command {
	c := &cobra.Command{Use: "recording", Short: "Inspect and export replay-ledger recordings"}
	c.AddCommand(newRecordingListCmd(), newRecordingExportCmd())
	return c
}

func newRecordingListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List recorded chat sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunRecordingList(cmd.Context(), cmd.OutOrStdout(), t, asJSON)
			})
		},
	}
	asJSONFlag(c, &asJSON)
	return c
}

// newRecordingExportCmd: `recording export <chat-id> [-o file]` - the
// dogfooding ritual for the replay engine (.quack/replay-log.md): hit a bug
// running quack → `quack recording export <chat-id>` → attach the zip to the
// issue, or drop it in testdata/ to pin it as a fixture.
func newRecordingExportCmd() *cobra.Command {
	var output string
	c := &cobra.Command{
		Use:   "export <chat-id>",
		Short: "Download a chat's replay-ledger recording bundle",
		Long: "Download a chat's replay-ledger recording bundle (a ZIP: manifest.json +\n" +
			"entries.jsonl + an optional clone.bundle) - default filename <chat-id>.zip.\n\n" +
			"Dogfooding ritual: hit a bug while running quack -> `quack recording export\n" +
			"<chat-id>` -> attach the zip to the issue, or drop it in testdata/ to pin\n" +
			"it as a fixture for the replay engine.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunRecordingExport(cmd.Context(), cmd.OutOrStdout(), t, args[0], output)
			})
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "output file path (default: <chat-id>.zip)")
	return c
}

func newNodeStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "stop <chat-id> <node-id>",
		Aliases: []string{"cancel"},
		Short:   "Stop a running node (the rest of the run continues)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeStop(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1])
			})
		},
	}
}

// newNodePauseCmd: `chat node pause` - suspend a RUNNING node at its next
// turn boundary, keeping its accumulated work (updateNodeStatus status=paused).
func newNodePauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <chat-id> <node-id>",
		Short: "Suspend a running node, keeping its accumulated work (resumable)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodePause(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1])
			})
		},
	}
}

// newNodeResumeCmd: `chat node resume` - resume a PAUSED node with a fresh
// re-run (updateNodeStatus status=running).
func newNodeResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <chat-id> <node-id>",
		Short: "Resume a paused node (a fresh re-run, like retry)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeResume(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1])
			})
		},
	}
}

// newNodeQueueCmd: `chat node queue` - append a message to a RUNNING node's
// queue, delivered at its next turn boundary (never mid-turn). Replaces the
// old interrupt-based `chat node steer`.
func newNodeQueueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "queue <chat-id> <node-id> <message>",
		Short: "Queue a message for a running node, delivered at its next turn boundary",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeQueue(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1], args[2])
			})
		},
	}
}

// newNodeQueueEditCmd: `chat node queue-edit` - rewrite a not-yet-delivered
// queued message.
func newNodeQueueEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "queue-edit <chat-id> <node-id> <message-id> <text>",
		Short: "Edit a not-yet-delivered queued message",
		Args:  cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeQueueEdit(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1], args[2], args[3])
			})
		},
	}
}

// newNodeQueueRemoveCmd: `chat node queue-remove` - drop a not-yet-delivered
// queued message.
func newNodeQueueRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "queue-remove <chat-id> <node-id> <message-id>",
		Short: "Remove a not-yet-delivered queued message",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeQueueRemove(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1], args[2])
			})
		},
	}
}

// newNodeEditCmd: `chat node edit` - replace a not-yet-started node's prompt
// (immutable once the node has started).
func newNodeEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <chat-id> <node-id> <task>",
		Short: "Edit a not-yet-started node's prompt",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeEditTask(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1], args[2])
			})
		},
	}
}

// newNodeRetryCmd: `chat node retry` - re-queue a finished node (and its
// downstream) with optional guidance (updateNodeStatus status=queued).
func newNodeRetryCmd() *cobra.Command {
	var guidance string
	c := &cobra.Command{
		Use:   "retry <chat-id> <node-id>",
		Short: "Re-run a finished node (done/failed/cancelled) and everything downstream of it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withTarget(cmd, func(t string) error {
				return cli.RunNodeRetry(cmd.Context(), cmd.OutOrStdout(), t, args[0], args[1], guidance)
			})
		},
	}
	c.Flags().StringVar(&guidance, "guidance", "", "extra guidance folded into the node's task for the re-run")
	return c
}

// newServerCmd: run and manage the server. `serve` folds in cmd/server in the
// next commit (it owns the SPA embed); the rest are management verbs.
func newServerCmd() *cobra.Command {
	c := &cobra.Command{Use: "server", Short: "Run and manage the quack server"}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the server in the foreground (REST + MCP API + embedded SPA)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfgPath, _ := cmd.Flags().GetString("config")
			port, _ := cmd.Flags().GetInt("port")
			return serve.Run(ctx, cfgPath, port)
		},
	}
	runCmd.Flags().String("config", defaultConfigPath(), "path to quack.yaml")
	runCmd.Flags().Int("port", 0, "listen port override (default: config server.addr, else 8080)")

	c.AddCommand(
		runCmd,
		newServerInitCmd(),
		newServerValidateCmd(),
		newServerUseCmd(),
		newServerAddCmd(),
		newServerListCmd(),
		newServerRemoveCmd(),
		newServerLoginCmd(),
	)
	return c
}

// newServerLoginCmd: `quack server login <name>` - OIDC login against a
// registered server's IdP. Separate from `server add` (which just records
// name→url) so a server that needs no auth never has to know about
// issuer/client-id, and re-login (token lost, revoked, or issuer rotated) is
// just re-running this one command.
func newServerLoginCmd() *cobra.Command {
	var scopes []string
	c := &cobra.Command{
		Use:   "login <name> --issuer <url> --client-id <id>",
		Short: "Log in to a registered server via OIDC (authorization code + PKCE)",
		Long: "Log in to a registered server's OIDC identity provider using the\n" +
			"authorization code flow with PKCE (RFC 6749 + RFC 7636): opens the\n" +
			"authorize URL in a browser (also printed, as a fallback) against a loopback\n" +
			"listener on an ephemeral port, then stores the access/refresh token on the\n" +
			"registered server so `quack chat`/`quack api`/`-p` attach it automatically.\n\n" +
			"Needs a browser reachable at 127.0.0.1 on this machine - it will not work\n" +
			"over a headless SSH session with no local browser or forwarded port.\n\n" +
			"Only public OIDC clients are supported (no client secret) - register a\n" +
			"PKCE-capable public client with your IdP for the quack CLI.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issuer, _ := cmd.Flags().GetString("issuer")
			clientID, _ := cmd.Flags().GetString("client-id")
			if issuer == "" || clientID == "" {
				return fmt.Errorf("--issuer and --client-id are required")
			}
			return cli.Login(cmd.Context(), cmd.OutOrStdout(), args[0], issuer, clientID, scopes)
		},
	}
	c.Flags().String("issuer", "", "OIDC issuer URL (required)")
	c.Flags().String("client-id", "", "OIDC public client id registered for the device authorization grant (required)")
	c.Flags().StringSliceVar(&scopes, "scopes", nil, "OAuth2 scopes to request (default: openid, profile, offline_access)")
	return c
}

// newServerValidateCmd: `quack server validate [path]` - loads and validates a
// quack.yaml (config.Load: parse, ${VAR} expand, validate) without starting the
// server. Same default-path resolution as `server run`'s --config.
func newServerValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Load and validate a quack.yaml without starting the server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := defaultConfigPath()
			if len(args) == 1 {
				path = args[0]
			}
			if _, err := config.Load(path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: OK\n", path)
			return nil
		},
	}
}

// newServerInitCmd: `quack server init` - the server-config wizard (LLM
// provider, models, stores). Writes quack.yaml.
func newServerInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Interactive setup wizard that writes quack.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, _ := cmd.Flags().GetString("output")
			force, _ := cmd.Flags().GetBool("force")
			if err := wizard.ServerInit(cmd.Context(), out, force); err != nil && !errors.Is(err, wizard.ErrAborted) {
				return err
			}
			return nil
		},
	}
	c.Flags().StringP("output", "o", "quack.yaml", "path to write quack.yaml")
	c.Flags().Bool("force", false, "overwrite an existing quack.yaml")
	return c
}

func newServerUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Switch the active server",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := cli.LoadClient()
			if err != nil {
				return err
			}
			if err := c.Use(args[0]); err != nil {
				return err
			}
			if err := c.Save(); err != nil {
				return err
			}
			fmt.Printf("active server: %s\n", args[0])
			return nil
		},
	}
}

func newServerAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Register a server",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := cli.LoadClient()
			if err != nil {
				return err
			}
			if err := c.AddServer(args[0], args[1]); err != nil {
				return err
			}
			if err := c.Save(); err != nil {
				return err
			}
			fmt.Printf("added %s (%s)\n", args[0], args[1])
			return nil
		},
	}
}

func newServerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured servers (* = active)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := cli.LoadClient()
			if err != nil {
				return err
			}
			if len(c.Servers) == 0 {
				fmt.Println("no servers registered - run `quack init` or `quack server add`")
				return nil
			}
			for name, s := range c.Servers {
				mark := " "
				if name == c.Active {
					mark = "*"
				}
				fmt.Printf("%s %-12s %s\n", mark, name, s.URL)
			}
			return nil
		},
	}
}

func newServerRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a registered server",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := cli.LoadClient()
			if err != nil {
				return err
			}
			if _, ok := c.Servers[args[0]]; !ok {
				return fmt.Errorf("server %q is not registered", args[0])
			}
			c.RemoveServer(args[0])
			if err := c.Save(); err != nil {
				return err
			}
			fmt.Printf("removed %s\n", args[0])
			return nil
		},
	}
}

// newAPICmd: raw REST passthrough, modeled on `gh api`. With one arg the method
// defaults to GET; the response body is written to stdout (pipeable to jq).
func newAPICmd() *cobra.Command {
	var data string
	c := &cobra.Command{
		Use:   "api [method] <path>",
		Short: "Make a raw request to the quack REST API (à la `gh api`)",
		Long: "Make a raw request to the quack REST API and print the response body.\n\n" +
			"  quack api /health\n" +
			"  quack api /api/v1/chats\n" +
			"  quack api POST /api/v1/chats -d '{\"system_prompt\":\"...\"}'\n" +
			"  quack api POST /api/v1/chats -d @body.json   # or -d - to read stdin\n\n" +
			"Targets the active server (or --server); with neither, runs the duck in-process.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, path := "GET", args[0]
			if len(args) == 2 {
				method, path = args[0], args[1]
			}
			body, err := apiBody(data, cmd.InOrStdin())
			if err != nil {
				return err
			}
			server, _ := cmd.Flags().GetString("server")
			target, stop, err := resolveTarget(cmd.Context(), server)
			if err != nil {
				return err
			}
			defer stop()
			return cli.RunAPI(cmd.Context(), cmd.OutOrStdout(), target, method, path, body)
		},
	}
	c.Flags().StringVarP(&data, "data", "d", "", "request body: literal string, @file, or - for stdin")
	return c
}

// apiBody resolves the -d value into a request body: "" → none, "-" → stdin,
// "@path" → file contents, else the literal string.
func apiBody(data string, stdin io.Reader) (io.Reader, error) {
	switch {
	case data == "":
		return nil, nil
	case data == "-":
		return stdin, nil
	case strings.HasPrefix(data, "@"):
		b, err := os.ReadFile(data[1:])
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(b), nil
	default:
		return strings.NewReader(data), nil
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the quack version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

// defaultConfigPath resolves the server config: $QUACK_CONFIG if set, else
// ./quack.yaml in cwd - matching what `quack init` writes, so init→run just
// works in a fresh dir. The repo's example config at config/quack.yaml is
// reached via --config (the Makefile's run target does this) or QUACK_CONFIG.
func defaultConfigPath() string {
	if p := os.Getenv("QUACK_CONFIG"); p != "" {
		return p
	}
	return "quack.yaml"
}

// resolveTarget returns the server base URL a client command should talk to, plus
// a stop func to call when done. If a remote is configured (--server override or
// an active registry entry) it's used as-is and stop is a no-op. Otherwise the
// duck is started in-process on a loopback port and stop tears it down - so the
// CLI works locally with no separate `quack server run`.
func resolveTarget(ctx context.Context, override string) (string, func(), error) {
	noop := func() {}
	cc, err := cli.LoadClient()
	if err != nil {
		return "", noop, err
	}
	if url := cc.ActiveURL(override); url != "" {
		return url, noop, nil // remote
	}
	cfg := defaultConfigPath()
	if _, err := os.Stat(cfg); err != nil {
		return "", noop, fmt.Errorf("no %s found - run `quack init` first (or pass --server <url>)", cfg)
	}
	base, stop, err := serve.InProcess(ctx, cfg)
	if err != nil {
		return "", noop, err
	}
	return base, func() { _ = stop() }, nil
}
