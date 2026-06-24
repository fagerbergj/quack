package serve

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

// Start launches `quack server run --config <configPath>` in the background,
// detached from this terminal, and waits until it is listening.
func Start(configPath string) error {
	if pid, ok := runningPID(); ok {
		return fmt.Errorf("quack server already running (pid %d); use `quack server stop` first", pid)
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

	cmd := exec.Command(self, "server", "run", "--config", configPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	if err := os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}

	// Wait until it's actually listening (or it dies during startup).
	addr := ":8080"
	if cfg, err := config.Load(configPath); err == nil && cfg.Server.Addr != "" {
		addr = cfg.Server.Addr
	}
	if err := waitListening(addr, cmd.Process.Pid, 15*time.Second); err != nil {
		return fmt.Errorf("server did not come up: %w — see %s", err, logPath())
	}
	fmt.Printf("quack server started (pid %d), listening on %s\n", cmd.Process.Pid, addr)
	return nil
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

// runningPID returns the pidfile's PID if it names a live process.
func runningPID() (int, bool) {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil || pid <= 0 {
		return 0, false
	}
	return pid, alive(pid)
}

// alive reports whether pid is a running process (signal 0 probes existence).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitListening blocks until addr accepts a TCP connection, failing fast if the
// process dies first.
func waitListening(addr string, pid int, timeout time.Duration) error {
	dialAddr := addr
	if len(addr) > 0 && addr[0] == ':' {
		dialAddr = "127.0.0.1" + addr
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return fmt.Errorf("process exited during startup")
		}
		c, err := net.DialTimeout("tcp", dialAddr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for %s", timeout, dialAddr)
}
