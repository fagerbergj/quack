package vetting

import (
	"os"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain answers the sandbox shim's self-exec (workspace.SandboxExecArg) before the test framework sees
// argv; otherwise the child runs as a test with nonsense flags and hangs until timeout.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	workspace.GitProtocol = "file" // fixtures are local bare repos
	// A per-run deadline would only measure host load; go test -timeout bounds a real hang.
	MermaidValidateTimeout = time.Hour
	os.Exit(m.Run())
}
