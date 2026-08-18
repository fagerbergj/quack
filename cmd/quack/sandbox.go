package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/workspace"
)

// sandboxFlags are the flags every quack sandbox form shares (issue #951's
// "[same flags]" on run/check/info).
type sandboxFlags struct {
	agent string
	cwd   string
	mode  string
	keep  bool
}

func addSandboxFlags(c *cobra.Command, f *sandboxFlags) {
	c.Flags().StringVar(&f.agent, "agent", "", "agent whose exact caps to use (default: "+cli.DefaultSandboxAgent+")")
	c.Flags().StringVar(&f.cwd, "cwd", "", "working dir inside the jail (default: a freshly minted node-shaped dir; \".\" jails the current dir)")
	c.Flags().StringVar(&f.mode, "mode", "", "override the configured sandbox (landlock|bwrap|none)")
	c.Flags().BoolVar(&f.keep, "keep", false, "skip scratch/cwd teardown on exit")
}

// newSandboxCmd: `quack sandbox` - construct the exact jail an ACP agent gets
// and either drop into an interactive shell inside it (no args) or run one
// of the one-shot forms (run/check/info). See docs/sandbox-cli.md.
func newSandboxCmd() *cobra.Command {
	var f sandboxFlags
	c := &cobra.Command{
		Use:   "sandbox",
		Short: "Enter or probe the real agent jail (same Caps, WrapArgv, spawnEnv as an ACP agent)",
		Long: "quack sandbox constructs EXACTLY the environment an ACP agent gets -\n" +
			"same workspace.Caps, same WrapArgv wrapping, same spawnEnv - and either\n" +
			"drops you into an interactive shell inside it (no args) or runs a\n" +
			"one-shot form (run/check/info). This is how you answer \"does this\n" +
			"pass in the reviewer's sandbox\" without dispatching an agent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxInteractive(cmd, f)
		},
	}
	addSandboxFlags(c, &f)
	c.AddCommand(newSandboxRunCmd(), newSandboxCheckCmd(), newSandboxInfoCmd())
	return c
}

// openSandboxSeat loads the local quack.yaml, opens the configured jail, and
// resolves a cli.SandboxSeat for f - the shared setup every sandbox form does
// first.
func openSandboxSeat(f sandboxFlags) (cli.SandboxSeat, func(), error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.LoadForSandbox(cfgPath)
	if err != nil {
		return cli.SandboxSeat{}, nil, err
	}
	jail, err := workspace.NewJail(cfg.Workspace.Root)
	if err != nil {
		return cli.SandboxSeat{}, nil, err
	}
	seat, err := cli.ResolveSandboxSeat(cfg, jail, f.agent, f.cwd, f.mode)
	if err != nil {
		return cli.SandboxSeat{}, nil, err
	}
	teardown := func() {
		if !f.keep {
			seat.Cleanup()
		}
	}
	return seat, teardown, nil
}

// sandboxAgentConfig re-resolves the AgentConfig for seat.AgentName - needed
// alongside the seat for SandboxSpawnEnv's acp.env merge (cli.SandboxSeat
// deliberately carries only Caps, not the full AgentConfig).
func sandboxAgentConfig(f sandboxFlags) (config.AgentConfig, error) {
	cfg, err := config.LoadForSandbox(defaultConfigPath())
	if err != nil {
		return config.AgentConfig{}, err
	}
	_, ac, err := cli.ResolveSandboxAgent(cfg, f.agent)
	return ac, err
}

// runSandboxInteractive: sh -i through the SAME wrappedArgv/spawnEnv path the
// ACP child gets (workspace.WrapArgv + cli.SandboxSpawnEnv), with a real pty
// crossing the sandbox boundary so job control and the prompt work under
// bwrap/landlock, not just `none`.
func runSandboxInteractive(cmd *cobra.Command, f sandboxFlags) error {
	seat, teardown, err := openSandboxSeat(f)
	if err != nil {
		return err
	}
	defer teardown()
	ac, err := sandboxAgentConfig(f)
	if err != nil {
		return err
	}

	shell := "sh"
	if sh := os.Getenv("SHELL"); sh != "" && shOnChildPath(seat.Caps, sh) {
		shell = sh
	}
	argv := workspace.WrapArgv(seat.Dir, []string{shell, "-i"}, seat.Caps, nil, nil)
	env := cli.SandboxSpawnEnv(seat.Caps, ac, map[string]string{
		"PS1":  cli.SandboxPS1(seat.AgentName, seat.ReadOnly),
		"TERM": envOr("TERM", "xterm-256color"),
	})

	fmt.Fprintf(cmd.ErrOrStderr(), "quack sandbox: %s (%s), cwd=%s\n", seat.AgentName, seat.Caps.Sandbox, seat.Dir)

	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = seat.Dir
	c.Env = env

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		// No tty available (e.g. piped stdin, non-interactive CI): a pty-less
		// interactive shell is the broken-prompt failure mode this issue calls
		// out explicitly - refuse instead of pretending it worked.
		return fmt.Errorf("quack sandbox: stdin is not a terminal; interactive mode needs one (docker exec needs -it). Use `quack sandbox run` for a non-interactive command")
	}

	ptmx, err := pty.Start(c)
	if err != nil {
		return fmt.Errorf("quack sandbox: start pty: %w", err)
	}
	defer ptmx.Close()

	resize := func() {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
			_ = pty.Setsize(ptmx, ws)
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			resize()
		}
	}()

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("quack sandbox: raw terminal: %w", err)
	}
	defer func() { _ = term.Restore(stdinFd, oldState) }()

	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
	_, _ = io.Copy(os.Stdout, ptmx)

	err = c.Wait()
	if exitErr, ok := err.(*exec.ExitError); ok {
		// os.Exit skips defers - restore the terminal and tear down the seat
		// before it, or a non-zero shell exit leaves the caller's tty raw.
		_ = term.Restore(stdinFd, oldState)
		teardown()
		os.Exit(exitErr.ExitCode())
	}
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// shOnChildPath reports whether sh (an absolute path, e.g. $SHELL) resolves
// on the hermetic ChildPath the sandboxed child actually gets - a $SHELL from
// the operator's ambient env may not exist inside the jail at all.
func shOnChildPath(caps workspace.Caps, shellPath string) bool {
	for _, dir := range strings.Split(workspace.ChildPath(caps), ":") {
		if dir == "" {
			continue
		}
		if fi, err := os.Stat(shellPath); err == nil && !fi.IsDir() && strings.HasPrefix(shellPath, dir+"/") {
			return true
		}
	}
	return false
}

// newSandboxRunCmd: `quack sandbox run [flags] "CMD ARGS"` - one shot, exit
// code passthrough, through sh -c "$ARG" wrapped the same way interactive is.
func newSandboxRunCmd() *cobra.Command {
	var f sandboxFlags
	c := &cobra.Command{
		Use:   "run [flags] \"CMD ARGS\"",
		Short: "Run one command in the jail; exit code and output pass through",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxRun(cmd, f, args[0])
		},
	}
	addSandboxFlags(c, &f)
	return c
}

func runSandboxRun(cmd *cobra.Command, f sandboxFlags, script string) error {
	seat, teardown, err := openSandboxSeat(f)
	if err != nil {
		return err
	}
	defer teardown()
	ac, err := sandboxAgentConfig(f)
	if err != nil {
		return err
	}

	argv := workspace.WrapArgv(seat.Dir, []string{"sh", "-c", script}, seat.Caps, nil, nil)
	env := cli.SandboxSpawnEnv(seat.Caps, ac, nil)

	c := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
	c.Dir = seat.Dir
	c.Env = env
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()

	err = c.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		teardown() // os.Exit below skips defer; run it before exiting
		exitIfNonZero(exitErr.ExitCode())
		return nil
	}
	return err
}

// cmdSandboxRunner adapts a resolved seat to cli.SandboxRunner for `check`'s
// probes - each probe is just another `sandbox run`.
type cmdSandboxRunner struct {
	seat cli.SandboxSeat
	ac   config.AgentConfig
}

func (r cmdSandboxRunner) Run(ctx context.Context, script string) (string, int, error) {
	argv := workspace.WrapArgv(r.seat.Dir, []string{"sh", "-c", script}, r.seat.Caps, nil, nil)
	env := cli.SandboxSpawnEnv(r.seat.Caps, r.ac, nil)
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = r.seat.Dir
	c.Env = env
	out, err := c.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		return string(out), -1, err
	}
	return string(out), code, nil
}

// newSandboxCheckCmd: `quack sandbox check [flags]` - the probe table, exit
// non-zero on any FAIL.
func newSandboxCheckCmd() *cobra.Command {
	var f sandboxFlags
	c := &cobra.Command{
		Use:   "check",
		Short: "Run the built-in jail probes; non-zero exit on any FAIL",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxCheck(cmd, f)
		},
	}
	addSandboxFlags(c, &f)
	return c
}

func runSandboxCheck(cmd *cobra.Command, f sandboxFlags) error {
	seat, teardown, err := openSandboxSeat(f)
	if err != nil {
		return err
	}
	defer teardown()
	cfg, err := config.LoadForSandbox(defaultConfigPath())
	if err != nil {
		return err
	}
	_, ac, err := cli.ResolveSandboxAgent(cfg, f.agent)
	if err != nil {
		return err
	}

	runner := cmdSandboxRunner{seat: seat, ac: ac}
	results := cli.RunSandboxChecks(cmd.Context(), runner, seat.ReadOnly, workspace.EnforcesBoundary(seat.Caps.Sandbox), cfg.Workspace.CheckCommands)
	fmt.Fprint(cmd.OutOrStdout(), cli.FormatSandboxProbeTable(results))
	if cli.AnyFail(results) {
		teardown() // os.Exit below skips defer; run it before exiting
		exitIfNonZero(1)
	}
	return nil
}

// newSandboxInfoCmd: `quack sandbox info [flags]` - resolved mode/cwd/tmp/
// home/grants/env, no exec.
func newSandboxInfoCmd() *cobra.Command {
	var f sandboxFlags
	c := &cobra.Command{
		Use:   "info",
		Short: "Print the resolved jail (mode, cwd, tmp, home, grants, env) without running anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxInfo(cmd, f)
		},
	}
	addSandboxFlags(c, &f)
	return c
}

func runSandboxInfo(cmd *cobra.Command, f sandboxFlags) error {
	seat, teardown, err := openSandboxSeat(f)
	if err != nil {
		return err
	}
	defer teardown()
	ac, err := sandboxAgentConfig(f)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	readWrite := "rw"
	if seat.ReadOnly {
		readWrite = "ro"
	}
	fmt.Fprintf(out, "agent:        %s (%s)\n", seat.AgentName, readWrite)
	fmt.Fprintf(out, "mode:         %s\n", seat.Caps.Sandbox)
	fmt.Fprintf(out, "cwd:          %s\n", seat.Dir)
	fmt.Fprintf(out, "tmp:          %s\n", workspace.SandboxTmpDir(seat.Caps))
	fmt.Fprintf(out, "home:         %s\n", seat.Caps.HomeDir)
	fmt.Fprintf(out, "path:         %s\n", workspace.ChildPath(seat.Caps))
	fmt.Fprintf(out, "ro grants:    %s\n", strings.Join(seat.Caps.ExtraRO, ", "))
	fmt.Fprintf(out, "rw grants:    %s\n", seat.Dir)
	fmt.Fprintln(out, "env:")
	for _, kv := range cli.SandboxSpawnEnv(seat.Caps, ac, nil) {
		fmt.Fprintf(out, "  %s\n", kv)
	}
	return nil
}
