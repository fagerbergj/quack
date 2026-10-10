package vetting

import (
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain answers the sandbox shim's self-exec (workspace.SandboxExecArg) before the test framework sees
// argv; otherwise the child runs as a test with nonsense flags and hangs until timeout.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	workspace.GitProtocol = "file" // fixtures are local bare repos
	os.Exit(m.Run())
}
