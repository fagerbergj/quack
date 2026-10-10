package workspace

import (
	"os"
	"testing"
)

// TestMain mirrors main()'s self-exec dispatch so tests can spawn the real shim from this test binary.
func TestMain(m *testing.M) {
	RunSandboxExecIfInvoked()
	os.Exit(m.Run())
}
