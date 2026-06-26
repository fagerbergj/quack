package serve

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fagerbergj/quack/internal/config"
)

// Background daemon control for `quack server start|stop`. start re-execs this
// binary as `server run` detached (its own session), records the PID, and waits
// for the listener; stop signals that PID. run (foreground) is unaffected.
// ponytail: one server per machine — a single pidfile under the state dir. Add a
// per-instance name if running several ever matters.

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "quack")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "quack")
}

func pidPath() string { return filepath.Join(stateDir(), "server.pid") }
func logPath() string { return filepath.Join(stateDir(), "server.log") }

// Start launches `quack server run` in the background, detached from this
// terminal, and waits until it is listening. port (non-zero) overrides config;
// the same override is passed to the child so the daemon and the wait agree.
// Under server.topology: managed it also brings up the stores stack first.
func Start(ctx context.Context, configPath string, port int) error {
	if pid, ok := runningPID(); ok {
		return fmt.Errorf("quack server already running (pid %d); use `quack server stop` first", pid)
	}
	addr := resolveAddr(configPath, port)
	// Catch a foreign listener (or a server we didn't start) before we fork.
	if listening(addr) {
		return fmt.Errorf("address %s is already in use — run `quack server status`, or pick another with --port", addr)
	}
	// Load config to check the topology. managed ⇒ bring up stores before forking
	// (the child `server run` re-runs upStores, which is idempotent).
	var topology string
	if cfg, err := config.Load(configPath); err == nil && cfg.Server.Managed() {
		topology = config.TopologyManaged
		if err := upStores(ctx); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open server log: %w", err)
	}
	defer logf.Close()

	args := []string{"server", "run", "--config", configPath}
	if port != 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	if err := writeState(cmd.Process.Pid, addr, topology); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	if err := waitListening(addr, cmd.Process.Pid, 15*time.Second); err != nil {
		return fmt.Errorf("server did not come up: %w — see %s", err, logPath())
	}
	fmt.Printf("quack server started (pid %d), listening on %s\n", cmd.Process.Pid, addr)
	return nil
}

// Status reports whether the server is running (our daemon or a foreign listener)
// and whether its address is accepting connections. The daemon's address comes
// from the recorded state, so it's correct even when started with --port.
func Status(configPath string, port int) error {
	pid, recAddr, topo, have := readState()
	ours := have && alive(pid)
	addr := recAddr
	if !ours || addr == "" {
		addr = resolveAddr(configPath, port)
	}
	switch {
	case ours && listening(addr):
		fmt.Printf("running — pid %d, listening on %s", pid, addr)
		if topo == config.TopologyManaged {
			fmt.Print(" (managed stores up)")
		}
		fmt.Println()
	case ours:
		fmt.Printf("running — pid %d, but %s is not accepting connections yet\n", pid, addr)
	case listening(addr):
		fmt.Printf("a server is listening on %s but was not started by `quack server start` (no live pidfile)\n", addr)
	default:
		fmt.Printf("stopped — nothing running, %s is free\n", addr)
	}
	return nil
}

// resolveAddr picks the listen address: a non-zero --port (":<port>"), else
// config's server.addr, else the :8080 default.
func resolveAddr(configPath string, port int) string {
	if port != 0 {
		return fmt.Sprintf(":%d", port)
	}
	if cfg, err := config.Load(configPath); err == nil && cfg.Server.Addr != "" {
		return cfg.Server.Addr
	}
	return ":8080"
}

// listening reports whether addr currently accepts a TCP connection.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", dialAddr(addr), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// dialAddr makes a listen address (":8080") dialable ("127.0.0.1:8080").
func dialAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// Stop signals the recorded server PID and waits for it to exit. Under the
// managed topology it also tears down the stores stack (volumes persist). The
// teardown covers both `server start` (topology recorded in state) and
// `server run` (no state — config is checked) origins. Returns nil if it
// stopped the app or tore down stores; errors only if nothing was running.
func Stop(configPath string) error {
	// Read state before removing the pidfile (we need the recorded topology).
	pid, _, stateTopo, _ := readState()
	haveApp := pid > 0 && alive(pid)
	if haveApp {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("signal pid %d: %w", pid, err)
		}
		for i := 0; i < 50 && alive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("quack server stopped (pid %d)\n", pid)
	}
	_ = os.Remove(pidPath()) // clear the pidfile (stale or fresh)
	// Tear down managed stores if this run used them: either the state recorded
	// managed (start origin) or the current config is managed (run origin, where
	// no pidfile was written). Only act when the stack is actually up, so the
	// "torn down" message and the success outcome are truthful.
	cfgManaged := false
	if cfg, err := config.Load(configPath); err == nil {
		cfgManaged = cfg.Server.Managed()
	}
	managed := stateTopo == config.TopologyManaged || cfgManaged
	toreDown := false
	if managed && storesUp() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := downStores(ctx); err != nil {
			// The app is already stopped; don't fail stop on a teardown hiccup,
			// just report it so the user can `docker compose -p quack-stores down`.
			fmt.Fprintf(os.Stderr, "warning: stores teardown failed: %v\n", err)
		} else {
			fmt.Println("managed stores torn down")
			toreDown = true
		}
	}
	if !haveApp && !toreDown {
		return fmt.Errorf("no running quack server (no live pid at %s)", pidPath())
	}
	return nil
}

// writeState records the daemon's PID, listen address, and topology
// ("PID ADDR TOPOLOGY"). topology is empty for non-managed runs.
func writeState(pid int, addr, topology string) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d %s %s\n", pid, addr, topology)), 0o644)
}

// readState parses the recorded PID, address, and topology. ok is false if
// absent/garbled. The topology field is optional (older state files omit it).
func readState() (pid int, addr, topology string, ok bool) {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, "", "", false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, "", "", false
	}
	if _, err := fmt.Sscanf(fields[0], "%d", &pid); err != nil || pid <= 0 {
		return 0, "", "", false
	}
	if len(fields) > 1 {
		addr = fields[1]
	}
	if len(fields) > 2 {
		topology = fields[2]
	}
	return pid, addr, topology, true
}

// runningPID returns the recorded PID if it names a live process.
func runningPID() (int, bool) {
	pid, _, _, ok := readState()
	return pid, ok && alive(pid)
}

// alive reports whether pid is a running process (signal 0 probes existence).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitListening blocks until addr accepts a TCP connection, failing fast if the
// process dies first.
func waitListening(addr string, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return fmt.Errorf("process exited during startup")
		}
		if listening(addr) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for %s", timeout, dialAddr(addr))
}
