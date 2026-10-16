package tools

import (
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain answers the __sandbox-exec self-exec a confined git call makes of this test binary.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	workspace.GitProtocol = "file" // fixtures are local bare repos
	os.Exit(m.Run())
}
