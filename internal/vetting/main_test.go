package vetting

import (
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain answers the sandbox shim's self-exec before the test framework sees
// the argv - a sandboxed child here re-execs THIS binary (see
// workspace.SandboxExecArg), and without this it would run as a test with
// nonsense flags and hang until the per-call timeout.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	os.Exit(m.Run())
}
