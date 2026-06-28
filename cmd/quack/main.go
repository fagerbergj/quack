// Command quack is Quack's one-binary CLI and server. `quack server run` runs the
// REST + MCP API and the embedded SPA; the other verbs (`chat`, `api`, `server`)
// drive a running server over HTTP + SSE. This file is the cobra wiring only —
// command funcs stay thin and dispatch into internal/cli and internal/tui (see
// the quack-cli skill); the leaf handlers are honest stubs until those land.
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
	"github.com/fagerbergj/quack/internal/serve"
	"github.com/fagerbergj/quack/internal/wizard"
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
			"  quack init              get configured (run locally or connect to a server)\n" +
			"  quack server run        run the API + SPA server (foreground)\n" +
			"  quack                   open the interactive chat TUI\n" +
			"  quack -p \"<prompt>\"      one-shot prompt, print and exit (no TUI)\n" +
			"  quack chat|server|api   manage chats, the server, and raw API calls",
		SilenceUsage: true, // a failing RunE is an error, not a usage mistake
		RunE: func(cmd *cobra.Command, args []string) error {
			if printPrompt == "" {
				return notWired("interactive TUI")
			}
			server, _ := cmd.Flags().GetString("server")
			target, stop, err := resolveTarget(cmd.Context(), server)
			if err != nil {
				return err
			}
			defer stop()
			return cli.PrintPrompt(cmd.Context(), cmd.OutOrStdout(), target, printPrompt)
		},
	}
	// Client context: which server to talk to. Resolution (flag → config → default)
	// lands with internal/cli; here it is just declared so subcommands inherit it.
	root.PersistentFlags().String("server", "", "quack server URL (default: active server from config, else http://localhost:8080)")
	root.Flags().StringVarP(&printPrompt, "print", "p", "", "one-shot prompt; print the result and exit (no TUI)")

	root.AddCommand(newInitCmd(), newChatCmd(), newServerCmd(), newAPICmd(), newVersionCmd())
	return root
}

// newInitCmd: `quack init` — onboarding. Local (run quack here → server wizard
// + register localhost) or Remote (register a server someone else runs).
func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Get quack configured — run locally or connect to a hosted server",
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
			port, _ := cmd.Flags().GetInt("port")
			return serve.Run(ctx, cfgPath, port)
		},
	}
	runCmd.Flags().String("config", defaultConfigPath(), "path to quack.yaml")
	runCmd.Flags().Int("port", 0, "listen port override (default: config server.addr, else 8080)")

	c.AddCommand(
		runCmd,
		newServerInitCmd(),
		newServerUseCmd(),
		newServerAddCmd(),
		newServerListCmd(),
		newServerRemoveCmd(),
	)
	return c
}

// newServerInitCmd: `quack server init` — the server-config wizard (LLM
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
				fmt.Println("no servers registered — run `quack init` or `quack server add`")
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

// defaultConfigPath resolves the server config: $QUACK_CONFIG if set, else
// ./quack.yaml in cwd — matching what `quack init` writes, so init→run just
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
// duck is started in-process on a loopback port and stop tears it down — so the
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
		return "", noop, fmt.Errorf("no %s found — run `quack init` first (or pass --server <url>)", cfg)
	}
	base, stop, err := serve.InProcess(ctx, cfg)
	if err != nil {
		return "", noop, err
	}
	return base, func() { _ = stop() }, nil
}
