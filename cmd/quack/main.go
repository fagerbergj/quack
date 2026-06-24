// Command quack is Quack's one-binary CLI and server. `quack server run` runs the
// REST + MCP API and the embedded SPA; the other verbs (`chat`, `api`, `server`)
// drive a running server over HTTP + SSE. This file is the cobra wiring only —
// command funcs stay thin and dispatch into internal/cli and internal/tui (see
// the quack-cli skill); the leaf handlers are honest stubs until those land.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/serve"
)

// version is the build stamp, overridden at release time via
//
//	-ldflags "-X main.version=v1.2.3"
//
// so `quack version` and the server report the same string (M10).
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1) // cobra has already printed the error to stderr
	}
}

func newRootCmd() *cobra.Command {
	var printPrompt string
	root := &cobra.Command{
		Use:   "quack",
		Short: "Quack — agentic research, one binary",
		Long: "Quack is a one-binary CLI and server for agentic research.\n\n" +
			"  quack server run       run the API + SPA server (foreground)\n" +
			"  quack                  open the interactive chat TUI\n" +
			"  quack -p \"<prompt>\"     one-shot prompt, print and exit (no TUI)\n" +
			"  quack chat|server|api  manage chats, the server, and raw API calls",
		SilenceUsage: true, // a failing RunE is an error, not a usage mistake
		RunE: func(cmd *cobra.Command, args []string) error {
			if printPrompt != "" {
				return notWired("print mode (-p)")
			}
			return notWired("interactive TUI")
		},
	}
	// Client context: which server to talk to. Resolution (flag → config → default)
	// lands with internal/cli; here it is just declared so subcommands inherit it.
	root.PersistentFlags().String("server", "", "quack server URL (default: active server from config, else http://localhost:8080)")
	root.Flags().StringVarP(&printPrompt, "print", "p", "", "one-shot prompt; print the result and exit (no TUI)")

	root.AddCommand(newChatCmd(), newServerCmd(), newAPICmd(), newVersionCmd())
	return root
}

// newChatCmd: create and drive chat sessions, plus per-node lifecycle control.
func newChatCmd() *cobra.Command {
	c := &cobra.Command{Use: "chat", Short: "Create and drive chat sessions"}

	node := &cobra.Command{Use: "node", Short: "Control nodes within a running chat"}
	node.AddCommand(
		stub("stop <node>", "Stop a running node", "chat node stop"),
		stub("cancel <node>", "Cancel a node", "chat node cancel"),
		stub("restart <node>", "Restart a node", "chat node restart"),
		stub("steer <node> <message>", "Steer a running node mid-run (HITL)", "chat node steer"),
	)

	c.AddCommand(
		stub("new [prompt]", "Start a new chat", "chat new"),
		stub("resume [id]", "Resume an existing chat", "chat resume"),
		stub("list", "List chats", "chat list"),
		stub("delete <id>", "Delete a chat", "chat delete"),
		stub("export <id>", "Export a chat transcript", "chat export"),
		stub("stop <id>", "Stop a running chat", "chat stop"),
		node,
	)
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
			addr, _ := cmd.Flags().GetString("addr")
			return serve.Run(ctx, cfgPath, addr)
		},
	}
	runCmd.Flags().String("config", defaultConfigPath(), "path to quack.yaml")
	runCmd.Flags().String("addr", "", "listen address override, e.g. :8081 (default: config server.addr)")

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the server in the background (detached)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			addr, _ := cmd.Flags().GetString("addr")
			return serve.Start(cfgPath, addr)
		},
	}
	startCmd.Flags().String("config", defaultConfigPath(), "path to quack.yaml")
	startCmd.Flags().String("addr", "", "listen address override, e.g. :8081 (default: config server.addr)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background server started by `quack server start`",
		RunE:  func(*cobra.Command, []string) error { return serve.Stop() },
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether a server is running and on what address",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			addr, _ := cmd.Flags().GetString("addr")
			return serve.Status(cfgPath, addr)
		},
	}
	statusCmd.Flags().String("config", defaultConfigPath(), "path to quack.yaml")
	statusCmd.Flags().String("addr", "", "address to check (default: recorded daemon addr, else config server.addr)")

	c.AddCommand(
		runCmd,
		startCmd,
		stopCmd,
		statusCmd,
		stub("init", "Interactive setup wizard that writes quack.yaml", "server init"),
		stub("use <name>", "Switch the active server", "server use"),
		stub("add <name> <url>", "Register a server", "server add"),
		stub("list", "List configured servers", "server list"),
	)
	return c
}

// newAPICmd: raw authenticated REST passthrough, modeled on `gh api`.
func newAPICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "api <method> <path>",
		Short: "Make an authenticated request to the quack REST API",
		RunE:  func(*cobra.Command, []string) error { return notWired("api passthrough") },
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

// stub builds a leaf command that reports it is not implemented yet. Removed as
// each verb gets wired to internal/cli / internal/tui.
func stub(use, short, what string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE:  func(*cobra.Command, []string) error { return notWired(what) },
	}
}

func notWired(what string) error {
	return fmt.Errorf("%s is not wired yet — coming in a later M8 commit", what)
}

// defaultConfigPath resolves the server config: $QUACK_CONFIG if set, else the
// conventional repo path.
func defaultConfigPath() string {
	if p := os.Getenv("QUACK_CONFIG"); p != "" {
		return p
	}
	return "config/quack.yaml"
}
