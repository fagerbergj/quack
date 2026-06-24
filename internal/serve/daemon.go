package serve

import (
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
func Start(configPath string, port int) error {
	if pid, ok := runningPID(); ok {
		return fmt.Errorf("quack server already running (pid %d); use `quack server stop` first", pid)
	}
	addr := resolveAddr(configPath, port)
	// Catch a foreign listener (or a server we didn't start) before we fork.
	if listening(addr) {
		return fmt.Errorf("address %s is already in use — run `quack server status`, or pick another with --port", addr)
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
	if err := writeState(cmd.Process.Pid, addr); err != nil {
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
// from the recorded state, so it's correct even when started with --addr.
func Status(configPath string, port int) error {
	pid, recAddr, have := readState()
	ours := have && alive(pid)
	addr := recAddr
	if !ours || addr == "" {
		addr = resolveAddr(configPath, port)
	}
	switch {
	case ours && listening(addr):
		fmt.Printf("running — pid %d, listening on %s\n", pid, addr)
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

// Stop signals the recorded server PID and waits for it to exit.
func Stop() error {
	pid, ok := runningPID()
	if !ok {
		_ = os.Remove(pidPath()) // clear a stale pidfile if present
		return fmt.Errorf("no running quack server (no live pid at %s)", pidPath())
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	for i := 0; i < 50 && alive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(pidPath())
	fmt.Printf("quack server stopped (pid %d)\n", pid)
	return nil
}

// writeState records the daemon's PID and listen address ("PID ADDR").
func writeState(pid int, addr string) error {
	return os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d %s\n", pid, addr)), 0o644)
}

// readState parses the recorded PID and address. ok is false if absent/garbled.
func readState() (pid int, addr string, ok bool) {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, "", false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, "", false
	}
	if _, err := fmt.Sscanf(fields[0], "%d", &pid); err != nil || pid <= 0 {
		return 0, "", false
	}
	if len(fields) > 1 {
		addr = fields[1]
	}
	return pid, addr, true
}

// runningPID returns the recorded PID if it names a live process.
func runningPID() (int, bool) {
	pid, _, ok := readState()
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
